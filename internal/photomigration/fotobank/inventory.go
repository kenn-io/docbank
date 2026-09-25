package fotobank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	kitbackup "go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/photomigration"
	docsqlite "go.kenn.io/docbank/sqlite"
)

var (
	ErrSourceRunning          = errors.New("fotobank source is running")
	ErrSourceWAL              = errors.New("fotobank source has unflushed WAL frames")
	ErrSchemaMismatch         = errors.New("fotobank catalog schema mismatch")
	ErrEmbeddedSchemaMismatch = errors.New("embedded Docbank schema mismatch")
)

type Request struct {
	CatalogPath     string
	VaultRoot       string
	ArchiveRoot     string
	SnapshotID      string
	OwnerMapPath    string
	DestinationRoot string
}

func Inventory(ctx context.Context, driver docsqlite.Driver, req Request) (photomigration.Report, error) {
	if err := docsqlite.Validate(driver); err != nil {
		return photomigration.Report{}, err
	}
	if req.OwnerMapPath == "" {
		return photomigration.Report{}, errors.New("owner map output path is required")
	}
	if req.ArchiveRoot != "" {
		return inventoryArchive(ctx, driver, req)
	}
	return inventoryInstall(ctx, driver, req)
}

func inventoryInstall(ctx context.Context, driver docsqlite.Driver, req Request) (photomigration.Report, error) {
	catalog, err := absoluteRegular(req.CatalogPath, "catalog")
	if err != nil {
		return photomigration.Report{}, err
	}
	vaultRoot, err := absoluteDir(req.VaultRoot, "vault root")
	if err != nil {
		return photomigration.Report{}, err
	}
	vaultDB := filepath.Join(vaultRoot, "docbank.db")
	if _, err := absoluteRegular(vaultDB, "embedded Docbank database"); err != nil {
		return photomigration.Report{}, err
	}
	if err := admitSidecars(catalog); err != nil {
		return photomigration.Report{}, err
	}
	if err := admitSidecars(vaultDB); err != nil {
		return photomigration.Report{}, err
	}
	hierarchy, err := (home.Layout{Root: vaultRoot}).TryLockExistingAncestors()
	if err != nil {
		return photomigration.Report{}, fmt.Errorf("locking Fotobank vault hierarchy: %w", err)
	}
	defer func() { _ = hierarchy.Release() }()
	catLock, err := openExistingCatalogLock(catalog + ".server.lock")
	if err != nil {
		return photomigration.Report{}, err
	}
	defer func() { _ = catLock.Unlock() }()
	if err := admitSidecars(catalog); err != nil {
		return photomigration.Report{}, err
	}
	if err := admitSidecars(vaultDB); err != nil {
		return photomigration.Report{}, err
	}
	db, err := driver.Open(catalog, docsqlite.OpenOptions{Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred})
	if err != nil {
		return photomigration.Report{}, fmt.Errorf("opening Fotobank catalog immutably: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if err := validateCatalog(ctx, db); err != nil {
		return photomigration.Report{}, err
	}
	report, entries, err := readCatalog(ctx, db, catalog, vaultRoot, driver)
	if err != nil {
		return photomigration.Report{}, err
	}
	if err := validateEmbeddedDocbank(ctx, driver, vaultDB); err != nil {
		return photomigration.Report{}, err
	}
	report.Schema.EmbeddedDocbankVersion = 16
	report.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	template, err := photomigration.NewOwnerMapTemplate(report, entries)
	if err != nil {
		return photomigration.Report{}, err
	}
	if err := photomigration.WriteOwnerMapTemplate(req.OwnerMapPath, template, catalog, vaultRoot, req.DestinationRoot); err != nil {
		return photomigration.Report{}, err
	}
	return report, nil
}

func inventoryArchive(ctx context.Context, driver docsqlite.Driver, req Request) (photomigration.Report, error) {
	repo, err := kitbackup.Open(req.ArchiveRoot)
	if err != nil {
		return photomigration.Report{}, fmt.Errorf("open Fotobank recovery archive: %w", err)
	}
	snapshotID := req.SnapshotID
	if snapshotID == "" {
		latest, latestErr := repo.LatestSnapshot()
		if latestErr != nil || latest == nil {
			if latestErr == nil {
				latestErr = errors.New("archive has no snapshots")
			}
			return photomigration.Report{}, latestErr
		}
		snapshotID = latest.SnapshotID
	}
	scratch, err := os.MkdirTemp("", "docbank-fotobank-inventory-")
	if err != nil {
		return photomigration.Report{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	catalog := filepath.Join(scratch, "catalog.sqlite")
	if err := backupapp.ReadSnapshotExtraFile(ctx, repo, snapshotID, "application/catalog.sqlite", catalog); err != nil {
		return photomigration.Report{}, err
	}
	if err := admitSidecars(catalog); err != nil {
		return photomigration.Report{}, err
	}
	db, err := driver.Open(catalog, docsqlite.OpenOptions{Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred})
	if err != nil {
		return photomigration.Report{}, fmt.Errorf("opening archived Fotobank catalog immutably: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if err := validateCatalog(ctx, db); err != nil {
		return photomigration.Report{}, err
	}
	report, entries, err := readCatalog(ctx, db, catalog, "", driver)
	if err != nil {
		return photomigration.Report{}, err
	}
	manifest, err := repo.LoadManifest(snapshotID)
	if err != nil {
		return photomigration.Report{}, fmt.Errorf("load archive manifest: %w", err)
	}
	if manifest.Metadata == nil || manifest.Metadata.Format != backupapp.MetadataFormat {
		format := ""
		if manifest.Metadata != nil {
			format = manifest.Metadata.Format
		}
		return photomigration.Report{}, fmt.Errorf("%w: archive metadata format %q, expected %q", ErrSchemaMismatch, format, backupapp.MetadataFormat)
	}
	if err := verifyArchiveMetadata(ctx, repo, manifest); err != nil {
		return photomigration.Report{}, fmt.Errorf("read archive metadata: %w", err)
	}
	format := manifest.Metadata.Format
	report.Source = photomigration.Source{Kind: photomigration.SourceArchive, Identity: snapshotID}
	report.Schema.ArchiveMetadataFormat = format
	report.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	template, err := photomigration.NewOwnerMapTemplate(report, entries)
	if err != nil {
		return photomigration.Report{}, err
	}
	if err := photomigration.WriteOwnerMapTemplate(req.OwnerMapPath, template, req.ArchiveRoot, req.DestinationRoot); err != nil {
		return photomigration.Report{}, err
	}
	return report, nil
}

func verifyArchiveMetadata(ctx context.Context, repo *kitbackup.Repo, manifest *kitbackup.Manifest) error {
	metadataID, err := pack.ParseBlobID(manifest.Metadata.Blob)
	if err != nil {
		return fmt.Errorf("parse metadata blob: %w", err)
	}
	known, err := repo.LoadBlobIndex()
	if err != nil {
		return fmt.Errorf("load metadata blob index: %w", err)
	}
	stream, err := repo.OpenBlob(ctx, known, metadataID, nil, packstore.PackExt)
	if err != nil {
		return fmt.Errorf("open metadata blob: %w", err)
	}
	if stream.Size() != manifest.Metadata.Bytes {
		_ = stream.Close()
		return fmt.Errorf("metadata blob size is %d, expected %d", stream.Size(), manifest.Metadata.Bytes)
	}
	readBytes, readErr := io.Copy(io.Discard, stream)
	verifyErr := stream.Verify()
	closeErr := stream.Close()
	if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
		return fmt.Errorf("verify metadata blob: %w", err)
	}
	if readBytes != manifest.Metadata.Bytes {
		return fmt.Errorf("metadata stream size is %d, expected %d", readBytes, manifest.Metadata.Bytes)
	}
	return nil
}

func admitSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		name := path + suffix
		info, err := os.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %s sidecar: %w", filepath.Base(name), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s sidecar is not regular", ErrSourceWAL, filepath.Base(name))
		}
		if suffix == "-wal" && info.Size() == 32 {
			continue
		}
		return fmt.Errorf("%w: %s is %d bytes", ErrSourceWAL, filepath.Base(name), info.Size())
	}
	return nil
}

func openExistingCatalogLock(path string) (*flock.Flock, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: catalog lifetime lock is absent", ErrSourceRunning)
		}
		return nil, fmt.Errorf("inspect catalog lifetime lock: %w", err)
	}
	lock := flock.New(path, flock.SetFlag(os.O_RDWR))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire catalog lifetime lock: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: catalog lifetime lock is held", ErrSourceRunning)
	}
	return lock, nil
}

func absoluteRegular(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", label)
	}
	return abs, nil
}

func absoluteDir(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", label)
	}
	return abs, nil
}

func fileIdentity(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}
