package backupapp

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/photomigration"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestReadSnapshotExtraFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "catalog.sqlite")
	want := []byte("verified archive extra")
	require.NoError(t, os.WriteFile(source, want, 0o600))
	repository, err := backup.Init(filepath.Join(root, "repository"))
	require.NoError(t, err)
	known, err := repository.LoadBlobIndex()
	require.NoError(t, err)
	appender := backup.NewPackAppender(repository, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	treeID, hasTree, err := backup.CaptureExtras(t.Context(), backup.ExtrasOptions{
		Spec: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: source, RecordAs: "application/catalog.sqlite"}}},
	}, appender)
	require.NoError(t, err)
	require.True(t, hasTree)
	packs, entries, err := appender.Finish()
	require.NoError(t, err)
	indexID, err := repository.WriteIndex(entries)
	require.NoError(t, err)
	snapshotID, err := repository.WriteManifest(&backup.Manifest{
		FormatVersion: 4, MinReaderVersion: 4, AppVersion: "backupapp-test",
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Extras:    backup.ManifestExtras{Tree: treeID.String()}, NewPacks: packs, NewIndex: indexID,
	})
	require.NoError(t, err)

	destination := filepath.Join(root, "out", "catalog.sqlite")
	require.NoError(t, ReadSnapshotExtraFile(t.Context(), repository, snapshotID, "application/catalog.sqlite", destination))
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, err = os.Stat(filepath.Join(root, "out", "other.sqlite"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSnapshotUniqueBlobBytes(t *testing.T) {
	metadata := []byte("{\"type\":\"meta\",\"format\":\"docbank-metadata\",\"version\":1,\"vault_id\":\"fixture\",\"node_sequence\":0}\n" +
		"{\"type\":\"blob\",\"hash\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"size\":7,\"created_at\":\"2026-01-01T00:00:00Z\"}\n" +
		"{\"type\":\"blob\",\"hash\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"size\":11,\"created_at\":\"2026-01-01T00:00:00Z\"}\n")
	repository, manifest := snapshotWithMetadata(t, metadata)

	got, err := SnapshotUniqueBlobBytes(t.Context(), repository, manifest)
	require.NoError(t, err)
	require.Equal(t, int64(18), got)
}

func TestSnapshotUniqueBlobBytesRejectsInvalidRecords(t *testing.T) {
	header := "{\"type\":\"meta\",\"format\":\"docbank-metadata\",\"version\":1,\"vault_id\":\"fixture\",\"node_sequence\":0}\n"
	for _, test := range []struct{ name, rows, wantError string }{
		{"empty hash", "{\"type\":\"blob\",\"hash\":\"\",\"size\":7}\n", "invalid blob record"},
		{"negative size", "{\"type\":\"blob\",\"hash\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"size\":-1}\n", "invalid blob record"},
		{"duplicate hash", "{\"type\":\"blob\",\"hash\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"size\":7}\n{\"type\":\"blob\",\"hash\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"size\":7}\n", "repeats blob"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, manifest := snapshotWithMetadata(t, []byte(header+test.rows))
			_, err := SnapshotUniqueBlobBytes(t.Context(), repository, manifest)
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func snapshotWithMetadata(t *testing.T, metadata []byte) (*backup.Repo, *backup.Manifest) {
	t.Helper()
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	known, err := repository.LoadBlobIndex()
	require.NoError(t, err)
	appender := backup.NewPackAppender(repository, known, pack.DefaultZstdLevel, nil, packstore.PackExt)
	metadataID, _, err := appender.Add(metadata)
	require.NoError(t, err)
	packs, entries, err := appender.Finish()
	require.NoError(t, err)
	indexID, err := repository.WriteIndex(entries)
	require.NoError(t, err)
	manifest := &backup.Manifest{
		FormatVersion: 4, MinReaderVersion: 4, AppVersion: "backupapp-test",
		CreatedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Metadata:  &backup.ManifestMetadata{Format: MetadataFormat, Blob: metadataID.String(), Bytes: int64(len(metadata))},
		NewPacks:  packs, NewIndex: indexID,
	}
	return repository, manifest
}

func TestPhotoMigrationBackupMetadataRoundTrip(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := store.Open(sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	report := photomigration.Report{
		Source:    photomigration.Source{Kind: photomigration.SourceInstall, Identity: "catalog-sha256"},
		Schema:    photomigration.Schema{CatalogVersion: 1, CatalogFingerprint: "fingerprint", EmbeddedDocbankVersion: 16},
		Counts:    photomigration.Counts{Owners: 1, Files: 1, Bytes: 10},
		Capacity:  photomigration.Capacity{SourceBytes: 10, UniqueBlobBytes: 10, MinimumContentBytes: 10},
		CreatedAt: "2026-09-25T00:00:00Z",
	}
	ownerMap, err := photomigration.NewOwnerMapTemplate(report, []photomigration.MapEntry{{
		SourceHub: "hub", SourceUserID: "user", StorageKey: "storage",
	}})
	require.NoError(t, err)
	run := store.PhotoMigrationRun{ID: uuid.NewString(), Source: report.Source, CreatedAt: report.CreatedAt, Report: report, OwnerMap: ownerMap}
	require.NoError(t, source.SavePhotoMigrationRun(t.Context(), run))
	db, err := source.SQLiteDriver().Open(sourcePath, sqlite.OpenOptions{
		Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred,
	})
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO photo_migration_map(run_id,source_table,source_id,destination_kind,destination_id,disposition)
		VALUES(?,?,?,?,?,?),(?,?,?,?,?,?)`,
		run.ID, "assets", "fotobank-asset-1", "photo_asset", "docbank-asset-1", store.PhotoMigrationDispositionMigrated,
		run.ID, "files", "fotobank-file-1", "content_version", "docbank-version-1", store.PhotoMigrationDispositionQuarantined)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	snapshot, err := NewMetadataSource(source).OpenSnapshot(t.Context())
	require.NoError(t, err)
	reader, size, err := snapshot.OpenMetadata(t.Context())
	require.NoError(t, err)
	metadata, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.NoError(t, snapshot.Close())
	require.Equal(t, int64(len(metadata)), size)
	require.Contains(t, string(metadata), `"type":"photo_migration_run"`)
	require.Contains(t, string(metadata), `"type":"photo_migration_map"`)

	targetPath := filepath.Join(t.TempDir(), "target.db")
	target, err := store.OpenForRestore(targetPath, source.SQLiteDriver())
	require.NoError(t, err)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(metadata)))
	restored, err := target.PhotoMigrationRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.Equal(t, run.OwnerMap, restored.OwnerMap)
	require.NoError(t, target.Close())
	check, err := source.SQLiteDriver().Open(targetPath, sqlite.OpenOptions{Access: sqlite.ReadOnlyImmutable})
	require.NoError(t, err)
	defer func() { _ = check.Close() }()
	for _, want := range []struct{ sourceTable, sourceID, destinationKind, destinationID, disposition string }{
		{"assets", "fotobank-asset-1", "photo_asset", "docbank-asset-1", store.PhotoMigrationDispositionMigrated},
		{"files", "fotobank-file-1", "content_version", "docbank-version-1", store.PhotoMigrationDispositionQuarantined},
	} {
		var got struct{ destinationKind, destinationID, disposition string }
		require.NoError(t, check.QueryRow(`SELECT destination_kind,destination_id,disposition FROM photo_migration_map WHERE run_id=? AND source_table=? AND source_id=?`,
			run.ID, want.sourceTable, want.sourceID).Scan(&got.destinationKind, &got.destinationID, &got.disposition))
		require.Equal(t, want.destinationKind, got.destinationKind)
		require.Equal(t, want.destinationID, got.destinationID)
		require.Equal(t, want.disposition, got.disposition)
	}
	var mapCount int
	require.NoError(t, check.QueryRow(`SELECT COUNT(*) FROM photo_migration_map WHERE run_id=?`, run.ID).Scan(&mapCount))
	require.Equal(t, 2, mapCount)
	require.True(t, strings.HasSuffix(string(metadata), "\n"))
}
