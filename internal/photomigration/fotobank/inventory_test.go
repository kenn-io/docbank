package fotobank

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/fotobanktest"
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
	report, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Owners != 1 || report.Counts.Assets != 1 || report.Counts.Files != 1 ||
		report.Counts.Bytes != 10 || report.Counts.Albums != 1 || report.Counts.Shares != 1 ||
		report.Counts.Checkouts != 1 || report.Counts.AIResults != 1 || report.Counts.HiddenSetup != 1 {
		t.Fatalf("unexpected inventory counts: %#v", report.Counts)
	}
	if report.Schema.CatalogVersion != 1 || report.Schema.EmbeddedDocbankVersion != 16 || len(report.Vectors) != 1 || !report.Vectors[0].Rebuildable {
		t.Fatalf("unexpected inventory schema: %#v", report)
	}
}

func TestFotobankInventorySourceUnchanged(t *testing.T) {
	fixture := createInventoryFixture(t)
	before, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath)); err != nil {
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
	fixture := createInventoryFixture(t)
	lock := flock.New(fixture.CatalogLock)
	ok, err := lock.TryLock()
	if err != nil || !ok {
		t.Fatalf("hold catalog lock: %v", err)
	}
	defer func() { _ = lock.Unlock() }()
	_, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrSourceRunning) {
		t.Fatalf("expected running-source refusal, got %v", err)
	}
}

func TestFotobankInventoryWAL(t *testing.T) {
	fixture := createInventoryFixture(t)
	wal := fixture.CatalogPath + "-wal"
	if err := os.WriteFile(wal, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath)); err != nil {
		t.Fatalf("32-byte WAL header should be admitted: %v", err)
	}
	_ = os.Remove(fixture.OwnerMapPath)
	if err := os.WriteFile(wal, make([]byte, 33), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrSourceWAL) {
		t.Fatalf("expected WAL refusal at 33 bytes, got %v", err)
	}
}

func TestFotobankCatalogFingerprint(t *testing.T) {
	fixture := createInventoryFixture(t)
	report, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
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
	_, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "unexpected_table") {
		t.Fatalf("expected named schema mismatch, got %v", err)
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
	_, err = Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath))
	if !errors.Is(err, ErrEmbeddedSchemaMismatch) || !strings.Contains(err.Error(), "17") {
		t.Fatalf("expected embedded schema mismatch, got %v", err)
	}
}

func TestFotobankVecIsRebuildable(t *testing.T) {
	fixture := createInventoryFixture(t)
	if _, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), inventoryRequest(fixture, fixture.OwnerMapPath)); err != nil {
		t.Fatal(err)
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
	report, err := Inventory(context.Background(), store.DefaultSQLiteDriver(), request)
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
	after, err := fixture.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("source digest changed from %s to %s", before, after)
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
	metadata := []byte("{}\n")
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
