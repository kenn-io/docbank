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
	OutputDir       string
	DestinationRoot string
}

// Result is the inventory report plus the two files written for it.
type Result struct {
	Report       photomigration.Report
	ReportPath   string
	OwnerMapPath string
}

func Inventory(ctx context.Context, driver docsqlite.Driver, req Request) (Result, error) {
	if err := docsqlite.Validate(driver); err != nil {
		return Result{}, err
	}
	if req.OutputDir == "" || !filepath.IsAbs(req.OutputDir) {
		return Result{}, errors.New("output directory must be absolute")
	}
	var report photomigration.Report
	var template photomigration.OwnerMapTemplate
	var sourceRoots []string
	var err error
	if req.ArchiveRoot != "" {
		if req.CatalogPath != "" || req.VaultRoot != "" {
			return Result{}, errors.New("archive inventory cannot include install paths")
		}
		archiveRoot, pathErr := absoluteDir(req.ArchiveRoot, "archive root")
		if pathErr != nil {
			return Result{}, pathErr
		}
		report, template, err = inventoryArchive(ctx, driver, archiveRoot)
		sourceRoots = []string{archiveRoot}
	} else {
		catalog, pathErr := absoluteRegular(req.CatalogPath, "catalog")
		if pathErr != nil {
			return Result{}, pathErr
		}
		vaultRoot, pathErr := absoluteDir(req.VaultRoot, "vault root")
		if pathErr != nil {
			return Result{}, pathErr
		}
		report, template, err = inventoryInstall(ctx, driver, catalog, vaultRoot)
		sourceRoots = []string{filepath.Dir(catalog), vaultRoot}
	}
	if err != nil {
		return Result{}, err
	}
	sourceRoots = append(sourceRoots, req.DestinationRoot)
	reportPath, ownerMapPath, err := photomigration.WriteInventoryFiles(req.OutputDir, report, template, sourceRoots...)
	if err != nil {
		return Result{}, err
	}
	return Result{Report: report, ReportPath: reportPath, OwnerMapPath: ownerMapPath}, nil
}

func inventoryInstall(ctx context.Context, driver docsqlite.Driver, catalog, vaultRoot string) (photomigration.Report, photomigration.OwnerMapTemplate, error) {
	vaultDB := filepath.Join(vaultRoot, "docbank.db")
	if _, err := absoluteRegular(vaultDB, "embedded Docbank database"); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	if err := admitSidecars(catalog); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	if err := admitSidecars(vaultDB); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	hierarchy, err := (home.Layout{Root: vaultRoot}).TryLockExistingAncestors()
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("locking Fotobank vault hierarchy: %w", err)
	}
	defer func() { _ = hierarchy.Release() }()
	catLock, err := openExistingCatalogLock(catalog + ".server.lock")
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	defer func() { _ = catLock.Unlock() }()
	if err := admitSidecars(catalog); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	if err := admitSidecars(vaultDB); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	db, err := driver.Open(catalog, docsqlite.OpenOptions{Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred})
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("opening Fotobank catalog immutably: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	version, layouts, err := validateCatalog(ctx, db)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	report, entries, err := readCatalog(ctx, db, catalog, version, layouts)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	uniqueBlobBytes, err := validateEmbeddedDocbank(ctx, driver, vaultDB)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	report.Capacity.UniqueBlobBytes = uniqueBlobBytes
	report.Capacity.MinimumContentBytes = uniqueBlobBytes
	report.Schema.EmbeddedDocbankVersion = 16
	report.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	template, err := photomigration.NewOwnerMapTemplate(report, entries)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	return report, template, nil
}

func inventoryArchive(ctx context.Context, driver docsqlite.Driver, archiveRoot string) (photomigration.Report, photomigration.OwnerMapTemplate, error) {
	repo, err := kitbackup.Open(archiveRoot)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("open Fotobank recovery archive: %w", err)
	}
	latest, err := repo.LatestSnapshot()
	if err != nil || latest == nil {
		if err == nil {
			err = errors.New("archive has no snapshots")
		}
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	snapshotID := latest.SnapshotID
	scratch, err := os.MkdirTemp("", "docbank-fotobank-inventory-")
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	catalog := filepath.Join(scratch, "catalog.sqlite")
	if err := backupapp.ReadSnapshotExtraFile(ctx, repo, snapshotID, "application/catalog.sqlite", catalog); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	if err := admitSidecars(catalog); err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	db, err := driver.Open(catalog, docsqlite.OpenOptions{Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred})
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("opening archived Fotobank catalog immutably: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	version, layouts, err := validateCatalog(ctx, db)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	report, entries, err := readCatalog(ctx, db, catalog, version, layouts)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	manifest, err := repo.LoadManifest(snapshotID)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("load archive manifest: %w", err)
	}
	if manifest.Metadata == nil || manifest.Metadata.Format != backupapp.MetadataFormat {
		format := ""
		if manifest.Metadata != nil {
			format = manifest.Metadata.Format
		}
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("%w: archive metadata format %q, expected %q", ErrSchemaMismatch, format, backupapp.MetadataFormat)
	}
	uniqueBlobBytes := manifest.Attachments.BlobBytes
	if uniqueBlobBytes < 0 {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, fmt.Errorf("archive manifest records negative blob bytes %d", uniqueBlobBytes)
	}
	report.Capacity.UniqueBlobBytes = uniqueBlobBytes
	report.Capacity.MinimumContentBytes = uniqueBlobBytes
	format := manifest.Metadata.Format
	report.Source = photomigration.Source{Kind: photomigration.SourceArchive, Identity: snapshotID}
	report.Schema.ArchiveMetadataFormat = format
	report.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	template, err := photomigration.NewOwnerMapTemplate(report, entries)
	if err != nil {
		return photomigration.Report{}, photomigration.OwnerMapTemplate{}, err
	}
	return report, template, nil
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
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s path must be absolute", label)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", label)
	}
	return path, nil
}

func absoluteDir(path, label string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s path must be absolute", label)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", label)
	}
	return path, nil
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
