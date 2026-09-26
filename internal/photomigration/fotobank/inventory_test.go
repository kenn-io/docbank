package fotobank

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/fotobanktest"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/photomigration"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
	kitbackup "go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func createInventoryFixture(t *testing.T) fotobanktest.Install {
	t.Helper()
	fixture, err := fotobanktest.CreateInstall(filepath.Join(t.TempDir(), "source"), store.DefaultSQLiteDriver())
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func inventoryRequest(fixture fotobanktest.Install, ownerMap string) Request {
	return Request{CatalogPath: fixture.CatalogPath, VaultRoot: fixture.VaultRoot,
		OwnerMapPath: ownerMap, DestinationRoot: filepath.Join(filepath.Dir(fixture.Root), "destination")}
}

func TestFotobankInventoryInstall(t *testing.T) {
	fixture := createInventoryFixture(t)
	report, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Owners != 1 || report.Counts.Assets != 1 || report.Counts.Files != 1 ||
		report.Counts.Bytes != 10 || report.Counts.Albums != 1 || report.Counts.AlbumMemberships != 1 || report.Counts.Shares != 1 ||
		report.Counts.Checkouts != 1 || report.Counts.CheckoutEntries != 1 || report.Counts.AIResults != 1 || report.Counts.HiddenSetup != 1 {
		t.Fatalf("unexpected inventory counts: %#v report %#v", report.Counts, report)
	}
	if report.Capacity.SourceBytes != 10 || report.Capacity.UniqueBlobBytes != 10 || report.Capacity.MinimumContentBytes != 10 {
		t.Fatalf("unexpected capacity estimate: %#v", report.Capacity)
	}
	if report.Schema.CatalogVersion != 1 || report.Schema.EmbeddedDocbankVersion != 16 || len(report.Vectors) != 1 ||
		report.Vectors[0].ID != 1 || report.Vectors[0].Fingerprint != "synthetic" || report.Vectors[0].State != "active" || !report.Vectors[0].Rebuildable {
		t.Fatalf("unexpected inventory schema: %#v", report)
	}
}

func TestFotobankInventoryCountsRetainedDocbankVersions(t *testing.T) {
	fixture := createInventoryFixture(t)
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(fixture.VaultRoot, "docbank.db"), sqliteWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blobs(hash,size,created_at) VALUES(?, ?, ?)`, strings.Repeat("e", 64), 10, "2026-01-02T00:00:00Z"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	report, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if err != nil {
		t.Fatal(err)
	}
	if report.Capacity.SourceBytes != 10 || report.Capacity.UniqueBlobBytes != 20 || report.Capacity.MinimumContentBytes != 20 {
		t.Fatalf("unexpected retained-version capacity estimate: %#v", report.Capacity)
	}
	after, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("source digest changed from %s to %s", before, after)
	}
}

func TestFotobankInventorySourceUnchanged(t *testing.T) {
	fixture := createInventoryFixture(t)
	before, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath)); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("source digest changed from %s to %s", before, after)
	}
}

func TestFotobankInventoryLocks(t *testing.T) {
	for _, test := range []struct {
		name, mode string
	}{
		{name: "catalog lifetime lock", mode: "catalog"},
		{name: "vault hierarchy lock", mode: "hierarchy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := createInventoryFixture(t)
			path := fixture.CatalogLock
			if test.mode == "hierarchy" {
				path = fixture.VaultRoot
			}
			holdInventoryLock(t, test.mode, path)
			_, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
			if err == nil {
				t.Fatal("expected running-source refusal")
			}
			if test.mode == "catalog" && !errors.Is(err, ErrSourceRunning) {
				t.Fatalf("expected typed running-source refusal, got %v", err)
			}
			if _, statErr := os.Stat(fixture.OwnerMapPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("refusal left owner-map output behind: %v", statErr)
			}
		})
	}

	t.Run("missing catalog lifetime lock", func(t *testing.T) {
		fixture := createInventoryFixture(t)
		if err := os.Remove(fixture.CatalogLock); err != nil {
			t.Fatal(err)
		}
		_, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
		if !errors.Is(err, ErrSourceRunning) {
			t.Fatalf("expected refusal for absent catalog lock, got %v", err)
		}
		if _, statErr := os.Stat(fixture.OwnerMapPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("refusal left owner-map output behind: %v", statErr)
		}
	})
}

func TestFotobankInventoryLockProcess(t *testing.T) {
	mode := os.Getenv("DOCBANK_FOTOBANK_LOCK_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("DOCBANK_FOTOBANK_LOCK_PATH")
	var release func() error
	switch mode {
	case "catalog":
		lock := flock.New(path)
		ok, err := lock.TryLock()
		if err != nil || !ok {
			t.Fatalf("acquire catalog lock: %v", err)
		}
		release = lock.Unlock
	case "hierarchy":
		lock, err := (home.Layout{Root: path}).TryLockExclusive()
		if err != nil {
			t.Fatalf("acquire vault hierarchy lock: %v", err)
		}
		release = lock.Release
	default:
		t.Fatalf("unknown lock mode %q", mode)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	releaseSignal := bufio.NewScanner(os.Stdin)
	if !releaseSignal.Scan() || releaseSignal.Text() != "release" {
		t.Fatal("lock helper did not receive release signal")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func holdInventoryLock(t *testing.T, mode, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	testBinary, err := os.Executable()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, testBinary, "-test.run=^TestFotobankInventoryLockProcess$", "-test.count=1")
	command.Env = append(os.Environ(), "DOCBANK_FOTOBANK_LOCK_MODE="+mode, "DOCBANK_FOTOBANK_LOCK_PATH="+path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	t.Cleanup(func() {
		if _, err := fmt.Fprintln(stdin, "release"); err != nil {
			t.Errorf("signal lock helper: %v", err)
		}
		_ = stdin.Close()
		for scanner.Scan() {
			// Drain the helper's test output before waiting for process exit.
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("read lock helper output: %v", err)
		}
		if err := command.Wait(); err != nil {
			t.Errorf("lock helper exited: %v: %s", err, stderr.String())
		}
		cancel()
	})
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("lock helper did not acquire lock: %v", scanner.Err())
	}
}

func TestFotobankInventoryWAL(t *testing.T) {
	fixture := createInventoryFixture(t)
	wal := fixture.CatalogPath + "-wal"
	if err := os.WriteFile(wal, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath)); err != nil {
		t.Fatalf("32-byte WAL header should be admitted: %v", err)
	}
	_ = os.Remove(fixture.OwnerMapPath)
	if err := os.WriteFile(wal, make([]byte, 33), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrSourceWAL) {
		t.Fatalf("expected WAL refusal at 33 bytes, got %v", err)
	}
}

func TestFotobankInventoryVaultSidecars(t *testing.T) {
	fixture := createInventoryFixture(t)
	vaultDB := filepath.Join(fixture.VaultRoot, "docbank.db")
	headerOnlyWAL := vaultDB + "-wal"
	if err := os.WriteFile(headerOnlyWAL, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath)); err != nil {
		t.Fatalf("32-byte embedded-database WAL header should be admitted: %v", err)
	}
	if err := os.Remove(fixture.OwnerMapPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(headerOnlyWAL); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		suffix string
		data   []byte
	}{
		{name: "WAL frames", suffix: "-wal", data: make([]byte, 33)},
		{name: "rollback journal", suffix: "-journal", data: []byte("journal")},
	} {
		t.Run(test.name, func(t *testing.T) {
			sidecar := vaultDB + test.suffix
			if err := os.WriteFile(sidecar, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
			if !errors.Is(err, ErrSourceWAL) {
				t.Fatalf("expected embedded database sidecar refusal, got %v", err)
			}
			if _, statErr := os.Stat(fixture.OwnerMapPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("sidecar refusal left owner-map output behind: %v", statErr)
			}
			if err := os.Remove(sidecar); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("non-regular WAL", func(t *testing.T) {
		wal := vaultDB + "-wal"
		if err := os.Mkdir(wal, 0o700); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(wal) }()
		_, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
		if !errors.Is(err, ErrSourceWAL) {
			t.Fatalf("expected non-regular sidecar refusal, got %v", err)
		}
	})
}

func TestFotobankCatalogFingerprint(t *testing.T) {
	fixture := createInventoryFixture(t)
	report, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if err != nil {
		t.Fatal(err)
	}
	if report.Schema.CatalogFingerprint != catalogFingerprint {
		t.Fatalf("got fingerprint %s, expected %s", report.Schema.CatalogFingerprint, catalogFingerprint)
	}
}

func TestFotobankSchemaMismatch(t *testing.T) {
	fixture := createInventoryFixture(t)
	db, err := store.DefaultSQLiteDriver().Open(fixture.CatalogPath, sqliteWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE unexpected_table(value TEXT)"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	_, _, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "unexpected_table") {
		t.Fatalf("expected named schema mismatch, got %v", err)
	}
	if _, err := os.Stat(fixture.OwnerMapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("schema refusal left owner-map output behind: %v", err)
	}
}

func TestEmbeddedDocbankSourceSchema(t *testing.T) {
	fixture := createInventoryFixture(t)
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(fixture.VaultRoot, "docbank.db"), sqliteWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE vault_metadata SET schema_version=17 WHERE singleton=1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	_, _, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrEmbeddedSchemaMismatch) || !strings.Contains(err.Error(), "17") {
		t.Fatalf("expected embedded schema mismatch, got %v", err)
	}
	if _, err := os.Stat(fixture.OwnerMapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("schema refusal left owner-map output behind: %v", err)
	}
}

func TestEmbeddedDocbankColumnMismatch(t *testing.T) {
	fixture := createInventoryFixture(t)
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(fixture.VaultRoot, "docbank.db"), sqliteWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE blobs RENAME COLUMN size TO bytes"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	_, _, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrEmbeddedSchemaMismatch) || !strings.Contains(err.Error(), "blobs columns") {
		t.Fatalf("expected embedded column mismatch, got %v", err)
	}
	if _, err := os.Stat(fixture.OwnerMapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("schema refusal left owner-map output behind: %v", err)
	}
}

func TestFotobankInventoryRefusesTemplateInsideSource(t *testing.T) {
	fixture := createInventoryFixture(t)
	before, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	request := inventoryRequest(fixture, filepath.Join(filepath.Dir(fixture.CatalogPath), "owner-map.json"))
	_, _, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), request)
	if err == nil {
		t.Fatal("expected owner-map path inside catalog source tree to be refused")
	}
	if _, statErr := os.Stat(request.OwnerMapPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refusal wrote into source tree: %v", statErr)
	}
	after, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("refusal changed source digest from %s to %s", before, after)
	}
}

func TestFotobankInventoryRejectsRelativeSourcePaths(t *testing.T) {
	fixture := createInventoryFixture(t)
	request := inventoryRequest(fixture, fixture.OwnerMapPath)
	request.CatalogPath = filepath.Base(request.CatalogPath)
	if _, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), request); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected relative catalog path refusal, got %v", err)
	}
	request = inventoryRequest(fixture, fixture.OwnerMapPath)
	request.VaultRoot = filepath.Base(request.VaultRoot)
	if _, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), request); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected relative vault root refusal, got %v", err)
	}
	if _, err := os.Stat(fixture.OwnerMapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relative path refusal left owner-map output behind: %v", err)
	}
}

func TestFotobankInventoryArchive(t *testing.T) {
	fixture := createInventoryFixture(t)
	archiveRoot, snapshotID := createInventoryArchive(t, fixture.CatalogPath)
	ownerMap := filepath.Join(t.TempDir(), "owner-map.json")
	request := Request{ArchiveRoot: archiveRoot, SnapshotID: snapshotID, OwnerMapPath: ownerMap,
		DestinationRoot: filepath.Join(filepath.Dir(archiveRoot), "destination")}

	before, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	archiveBefore, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	if err != nil {
		t.Fatal(err)
	}
	report, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), request)
	if err != nil {
		t.Fatal(err)
	}
	if report.Source.Kind != photomigration.SourceArchive || report.Source.Identity != snapshotID {
		t.Fatalf("unexpected archive identity: %#v", report.Source)
	}
	if report.Schema.ArchiveMetadataFormat != backupapp.MetadataFormat {
		t.Fatalf("unexpected archive metadata format: %q", report.Schema.ArchiveMetadataFormat)
	}
	if report.Counts.Owners != 1 || report.Counts.Files != 1 {
		t.Fatalf("unexpected archive counts: %#v", report.Counts)
	}
	if report.Capacity.UniqueBlobBytes != 10 || report.Capacity.MinimumContentBytes != 10 {
		t.Fatalf("unexpected archive capacity: %#v", report.Capacity)
	}
	after, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("source digest changed from %s to %s", before, after)
	}
	archiveAfter, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	if err != nil {
		t.Fatal(err)
	}
	if archiveBefore != archiveAfter {
		t.Fatalf("archive digest changed from %s to %s", archiveBefore, archiveAfter)
	}
	relativeRequest := request
	relativeRequest.ArchiveRoot = filepath.Base(archiveRoot)
	relativeRequest.OwnerMapPath = filepath.Join(t.TempDir(), "relative-archive-owner-map.json")
	if _, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), relativeRequest); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected relative archive path refusal, got %v", err)
	}
}

func TestFotobankInventoryPinnedRecoveryArchive(t *testing.T) {
	archiveRoot, err := filepath.Abs(filepath.Join("testdata", "fotobank-kit-v0.24.1"))
	if err != nil {
		t.Fatal(err)
	}
	archiveBefore, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	if err != nil {
		t.Fatal(err)
	}
	ownerMap := filepath.Join(t.TempDir(), "owner-map.json")
	report, _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), Request{
		ArchiveRoot: archiveRoot, OwnerMapPath: ownerMap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Source.Kind != photomigration.SourceArchive || report.Schema.ArchiveMetadataFormat != backupapp.MetadataFormat {
		t.Fatalf("unexpected pinned archive identity: %#v", report)
	}
	if report.Counts.Owners != 1 || report.Counts.Files != 1 || report.Capacity.UniqueBlobBytes != int64(len("synthetic original from pinned Fotobank Kit")) {
		t.Fatalf("unexpected pinned Fotobank archive inventory: %#v", report)
	}
	archiveAfter, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	if err != nil {
		t.Fatal(err)
	}
	if archiveBefore != archiveAfter {
		t.Fatalf("pinned archive digest changed from %s to %s", archiveBefore, archiveAfter)
	}
}

func createInventoryArchive(t *testing.T, catalogPath string) (string, string) {
	t.Helper()
	archiveRoot := filepath.Join(t.TempDir(), "archive")
	repo, err := kitbackup.Init(archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	known, err := repo.LoadBlobIndex()
	if err != nil {
		t.Fatal(err)
	}
	appender := kitbackup.NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	treeID, hasTree, err := kitbackup.CaptureExtras(t.Context(), kitbackup.ExtrasOptions{
		Spec: kitbackup.ExtrasSpec{Files: []kitbackup.ExtrasFileSpec{{Path: catalogPath, RecordAs: "application/catalog.sqlite"}}},
	}, appender)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTree {
		t.Fatal("archive extras tree was not captured")
	}
	metadata := []byte("{\"type\":\"meta\",\"format\":\"docbank-metadata\",\"version\":1,\"vault_id\":\"dddddddd-dddd-4ddd-8ddd-dddddddddddd\",\"node_sequence\":1}\n" +
		"{\"type\":\"blob\",\"hash\":\"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\",\"size\":10,\"created_at\":\"2026-01-01T00:00:00.000000000Z\"}\n")
	metadataID, _, err := appender.Add(metadata)
	if err != nil {
		t.Fatal(err)
	}
	packs, entries, err := appender.Finish()
	if err != nil {
		t.Fatal(err)
	}
	indexID, err := repo.WriteIndex(entries)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &kitbackup.Manifest{
		FormatVersion: 4, MinReaderVersion: 4, AppVersion: "fotobank-test",
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Metadata:  &kitbackup.ManifestMetadata{Format: backupapp.MetadataFormat, Blob: metadataID.String(), Bytes: int64(len(metadata))},
		Extras:    kitbackup.ManifestExtras{Tree: treeID.String()},
		NewPacks:  packs, NewIndex: indexID,
		Attachments: kitbackup.ManifestAttachments{Layout: []string{}, Recipes: []string{}, Lists: []string{}},
		Excluded:    []string{}, Stats: json.RawMessage(`{}`),
	}
	snapshotID, err := repo.WriteManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return archiveRoot, snapshotID
}

func sqliteWriteOptions() sqlite.OpenOptions {
	return sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred}
}
