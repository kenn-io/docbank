package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/photomigration"
)

func migrationTestRun() PhotoMigrationRun {
	report := photomigration.Report{
		Source:    photomigration.Source{Kind: photomigration.SourceInstall, Identity: "catalog-sha256"},
		Schema:    photomigration.Schema{CatalogVersion: 1, CatalogFingerprint: "fingerprint", EmbeddedDocbankVersion: 16},
		Counts:    photomigration.Counts{Owners: 1, Assets: 1, Files: 1, Bytes: 10},
		Capacity:  photomigration.Capacity{SourceBytes: 10, UniqueBlobBytes: 10, MinimumContentBytes: 10},
		CreatedAt: "2026-09-25T00:00:00Z",
	}
	ownerMap, err := photomigration.NewOwnerMapTemplate(report, []photomigration.MapEntry{{SourceHub: "hub", SourceUserID: "user", StorageKey: "storage"}})
	if err != nil {
		panic(err)
	}
	return PhotoMigrationRun{ID: uuid.NewString(), Source: report.Source, CreatedAt: report.CreatedAt, Report: report, OwnerMap: ownerMap}
}

func TestPhotoMigrationJSONLRoundTrip(t *testing.T) {
	ctx := context.Background()
	source, err := Open(t.TempDir() + "/source.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	run := migrationTestRun()
	if err := source.SavePhotoMigrationRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := source.db.Exec(`INSERT INTO photo_migration_map(run_id,source_hub,source_user_id,storage_key,state) VALUES(?,?,?,?,?)`, run.ID, "hub", "user", "storage", PhotoMigrationStateRebuildable); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err := source.ExportMetadata(ctx, &exported); err != nil {
		t.Fatal(err)
	}
	target, err := OpenForRestore(t.TempDir()+"/target.db", source.driver)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	if err := target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := target.PhotoMigrationRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := photomigration.EncodeReport(run.Report)
	right, _ := photomigration.EncodeReport(got.Report)
	if !bytes.Equal(left, right) || got.OwnerMap.Entries[0].StorageKey != "storage" {
		t.Fatalf("migration run did not round-trip: %#v", got)
	}
	var maps int
	if err := target.db.QueryRow(`SELECT COUNT(*) FROM photo_migration_map`).Scan(&maps); err != nil {
		t.Fatal(err)
	}
	if maps != 1 {
		t.Fatalf("got %d migration map rows, expected 1", maps)
	}
}

func TestPhotoMigrationAuditedVault(t *testing.T) {
	s, err := Open(t.TempDir() + "/audited.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	target, err := s.Mkdir(ctx, s.RootID(), "Photos")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewInitialAudit(ctx, target.ID, "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableInitialAudit(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePhotoMigrationRun(ctx, migrationTestRun()); err != nil {
		t.Fatal(err)
	}
	verified, err := s.VerifyAudit(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Evidence.Enabled {
		t.Fatal("expected an enabled audit chain")
	}
	if err := s.ValidateMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	assertAuditMetadataRoundTrip(t, s)
}
