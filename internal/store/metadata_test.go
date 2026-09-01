package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
	"go.kenn.io/kit/packstore"
)

const (
	metadataHashCurrent    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metadataHashTrashed    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	metadataHashVersion    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	metadataVersionCurrent = "11111111-1111-4111-8111-111111111111"
	metadataVersionOld     = "22222222-2222-4222-8222-222222222222"
	metadataVersionTrashed = "33333333-3333-4333-8333-333333333333"
	metadataIngestID       = "44444444-4444-4444-8444-444444444444"
	metadataTagID          = "55555555-5555-4555-8555-555555555555"
)

func TestExportMetadataPreservesV1JavaScriptSeparatorEscapes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	_, err = s.Mkdir(t.Context(), s.RootID(), "line\u2028paragraph\u2029")
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	assert.Contains(t, exported.String(), `"name":"line\u2028paragraph\u2029"`)
	assert.NotContains(t, exported.String(), "line\u2028paragraph\u2029")
}

func TestAuxiliaryChecksumMetadataRoundTripsAndRejectsMalformedMD5(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		driver docsqlite.Driver
	}{
		{name: "default", driver: DefaultSQLiteDriver()},
		{name: "pure Go", driver: modernc.Driver{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source, err := Open(filepath.Join(t.TempDir(), "source.db"), testCase.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			const md5sum = "f6fdffe48c908deb0f4c3bd36c032e72"
			_, err = source.CreateFile(t.Context(), source.RootID(), "source.bin",
				metadataHashCurrent, 9, "application/octet-stream", BlobPhysical{
					Encoding: "raw", StoredBytes: 9, PackEligible: true,
					Created: true, MD5: md5sum,
				})
			require.NoError(t, err)
			var exported bytes.Buffer
			require.NoError(t, source.ExportMetadata(t.Context(), &exported))
			assert.Contains(t, exported.String(),
				`{"type":"blob_checksum","blob_sha256":"`+metadataHashCurrent+`","md5":"`+md5sum+`"}`)

			target, err := Open(filepath.Join(t.TempDir(), "target.db"), testCase.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
			record, err := target.BlobChecksums(t.Context(), metadataHashCurrent)
			require.NoError(t, err)
			assert.Equal(t, md5sum, record.MD5)

			for _, invalid := range []struct {
				name string
				md5  string
			}{
				{name: "uppercase", md5: strings.ToUpper(md5sum)},
				{name: "empty", md5: ""},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					malformed := strings.Replace(exported.String(), md5sum, invalid.md5, 1)
					rejected, err := Open(filepath.Join(t.TempDir(), "rejected.db"), testCase.driver)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, rejected.Close()) })
					err = rejected.ImportMetadata(t.Context(), strings.NewReader(malformed))
					require.ErrorContains(t, err, "canonical lowercase MD5")
					_, err = rejected.BlobChecksums(t.Context(), metadataHashCurrent)
					require.ErrorIs(t, err, ErrNotFound)
				})
			}
		})
	}
}

func TestBackupExcludesUncommittedProviderStaging(t *testing.T) {
	s := newTestStore(t)
	staged := fakeHash("92")
	require.NoError(t, s.RecordRenditionBlob(t.Context(), staged, 9,
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true, Created: true}))
	var stagedCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_blob_staging
		WHERE blob_hash=?`, staged).Scan(&stagedCount))
	require.Equal(t, 1, stagedCount)

	var exported bytes.Buffer
	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var authorized int
	require.NoError(t, snapshot.QueryRowContext(t.Context(), BackupBlobAuthorityCTE+
		`SELECT COUNT(*) FROM backup_authorized_blobs WHERE hash=?`, staged).Scan(&authorized))
	require.Zero(t, authorized)
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	assert.NotContains(t, exported.String(), staged,
		"uncommitted provider staging is not logical backup authority")
}

func TestBackupLiveReferenceOverridesProviderStagingHashCollision(t *testing.T) {
	s := newTestStore(t)
	shared := fakeHash("93")
	node, err := s.CreateFile(t.Context(), s.RootID(), "durable.txt", shared, 9, "text/plain")
	require.NoError(t, err)
	require.NotEmpty(t, node.CurrentVersionID)
	require.NoError(t, s.RecordRenditionBlob(t.Context(), shared, 9,
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true, Created: false}))
	var staged int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_blob_staging
		WHERE blob_hash=?`, shared).Scan(&staged))
	require.Zero(t, staged, "deduplicated ordinary bytes are not provider staging")
	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	assert.Contains(t, exported.String(), shared,
		"provider deduplication must not erase a live content blob")
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForBackupRestore(
		t.Context(), bytes.NewReader(exported.Bytes())))
	has, err := restored.HasBlob(t.Context(), shared)
	require.NoError(t, err)
	assert.True(t, has, "live content hash-collision authority must survive restore")
}

func TestBackupExcludesCrashPendingDerivativeErasureAcrossMetadataRoundTrip(t *testing.T) {
	s := newTestStore(t)
	pending := fakeHash("95")
	require.NoError(t, s.RecordRenditionBlob(t.Context(), pending, 12,
		BlobPhysical{Encoding: "raw", StoredBytes: 12, PackEligible: true, Created: true}))
	purged, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{pending}, purged.PhysicalDerivativeBlobsPendingGC)
	_, err = s.db.Exec(`DELETE FROM rendition_blob_staging WHERE blob_hash=?`, pending)
	require.NoError(t, err,
		"simulate a committed derivative whose manifest was removed before physical collection")

	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var authorized int
	require.NoError(t, snapshot.QueryRowContext(t.Context(), BackupBlobAuthorityCTE+
		`SELECT COUNT(*) FROM backup_authorized_blobs WHERE hash=?`, pending).Scan(&authorized))
	require.Zero(t, authorized)
	var exported bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	assert.NotContains(t, exported.String(), pending)

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForBackupRestore(
		t.Context(), bytes.NewReader(exported.Bytes())))
	has, err := restored.HasBlob(t.Context(), pending)
	require.NoError(t, err)
	assert.False(t, has, "restore must not resurrect a logically purged derivative")
}

func TestMetadataJSONLRoundTripPreservesLogicalState(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	sourceVaultID := source.VaultID()
	require.NoError(t, validateUUIDv4(sourceVaultID))
	seedMetadataRoundTrip(t, source)
	watchRun, err := source.BeginIngest(ctx, "watch", "sessions")
	require.NoError(t, err)
	watched, err := source.IngestFileExact(
		ctx, watchRun, source.RootID(), "session.jsonl", metadataHashVersion, 9,
		"application/json", "daily/session.jsonl", "",
	)
	require.NoError(t, err)
	filesystemMTime := time.Date(2026, time.February, 3, 4, 5, 6, 120_000_000, time.UTC).
		Format(time.RFC3339Nano)
	ingestID, err := source.BeginIngest(ctx, "cli", "actual filesystem timestamp")
	require.NoError(t, err)
	_, added, err := source.IngestFile(ctx, ingestID, source.RootID(), "filesystem-time.txt",
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 4,
		"text/plain", "/source/filesystem-time.txt", filesystemMTime)
	require.NoError(t, err)
	require.True(t, added)
	lostParent, err := source.Mkdir(ctx, source.RootID(), "deleted-parent")
	require.NoError(t, err)
	lostFile, err := source.CreateFile(ctx, lostParent.ID, "lost.txt",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 6, "text/plain")
	require.NoError(t, err)
	_, _, err = source.Trash(ctx, lostFile.ID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = source.Trash(ctx, lostParent.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = source.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, lostParent.ID)
	require.NoError(t, err)
	require.NoError(t, source.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at)
			VALUES(100,1,'later-deleted','dir','2026-02-04T00:00:00.000000000Z','2026-02-04T00:00:00.000000000Z')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=100`)
		return err
	}))

	var first, second bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &first))
	require.NoError(t, source.ExportMetadata(ctx, &second))
	assert.Equal(t, first.Bytes(), second.Bytes(), "unchanged metadata must export byte-identically")
	assert.Contains(t, first.String(), `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"`+
		sourceVaultID+`","node_sequence":100}`)
	assert.Contains(t, first.String(), `"original_mtime":"2026-02-03T04:05:06.12Z"`)
	assert.Contains(t, first.String(), `"type":"provenance","identity":"`)
	assert.Contains(t, first.String(), `{"type":"watch_source","watch_name":"sessions","source_ref":"daily/session.jsonl"`)
	assert.Contains(t, first.String(), `"supersedes":null`)
	assert.Contains(t, first.String(), `{"type":"node","id":7,"parent_id":1,"name":"Projects","kind":"dir"`)
	assert.NotContains(t, first.String(), "blob_pack_entries")
	assert.NotContains(t, first.String(), "metadata-pack")

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))
	assert.Equal(t, sourceVaultID, target.VaultID())
	restoredWatched, noVersion, changed, err := target.SyncWatchedContent(
		ctx, "sessions", "daily/session.jsonl", metadataHashVersion, 9, "application/json",
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, watched.ID, restoredWatched.ID)
	assert.Empty(t, noVersion.ID)
	importedTag, err := target.TagByID(ctx, metadataTagID)
	require.NoError(t, err)
	assert.Equal(t, int64(7), importedTag.Revision)

	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, first.Bytes(), restored.Bytes())
	assert.Equal(t, int64(1), target.RootID())
	var importedTrashParent sql.NullInt64
	var importedTrashName sql.NullString
	require.NoError(t, target.db.QueryRowContext(ctx,
		`SELECT trash_parent, trash_name FROM nodes WHERE id = ?`, lostFile.ID).
		Scan(&importedTrashParent, &importedTrashName))
	assert.False(t, importedTrashParent.Valid)
	require.True(t, importedTrashName.Valid)
	assert.Equal(t, "lost.txt", importedTrashName.String)

	node, err := target.NodeByPath(ctx, "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(10), node.ID)
	assert.Equal(t, metadataHashCurrent, node.BlobHash)
	assert.Equal(t, metadataVersionCurrent, node.CurrentVersionID)
	versions, total, err := target.ContentVersions(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, metadataVersionCurrent, versions[0].ID)
	assert.Equal(t, metadataVersionOld, versions[1].ID)
	_, err = target.NodeByPath(ctx, "/Empty")
	require.NoError(t, err, "empty directories are logical backup records")

	results, truncated, err := target.SearchPage(ctx, "report", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, results, 1, "FTS must be rebuilt by logical node import")
	assert.Equal(t, node.ID, results[0].Node.ID)
	results, truncated, err = target.SearchPage(ctx, "line", 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, results, 1, "content FTS must be rebuilt from extracted-text metadata")
	assert.Equal(t, node.ID, results[0].Node.ID)
	assert.Equal(t, SearchMatchContent, results[0].Match)

	var packRows int64
	require.NoError(t, target.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM blob_packs)
		      + (SELECT COUNT(*) FROM blob_pack_entries)`).Scan(&packRows))
	assert.Zero(t, packRows, "physical pack authority is reconstructed separately")
	restoredLostFile, _, err := target.Restore(ctx, lostFile.ID, UnconditionalRev)
	require.NoError(t, err)
	restoredLostPath, err := target.Path(ctx, restoredLostFile.ID)
	require.NoError(t, err)
	assert.Equal(t, "/lost.txt", restoredLostPath)

	created, err := target.Mkdir(ctx, target.RootID(), "after-restore")
	require.NoError(t, err)
	assert.Greater(t, created.ID, int64(100), "AUTOINCREMENT must not reuse any historically allocated ID")
}

func TestMetadataImportQueuesOnlyCurrentTextVersions(t *testing.T) {
	ctx := t.Context()
	source := newTestStore(t)
	created, err := source.CreateFile(
		ctx, source.RootID(), "notes.txt", fakeHash("71"), 4, "text/plain",
	)
	require.NoError(t, err)
	current, _, err := source.ReplaceContent(
		ctx, created.ID, created.Revision, fakeHash("72"), 5, "text/plain",
	)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))

	pending, err := target.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, current.BlobHash, pending[0].BlobHash)
	assert.NotEqual(t, created.BlobHash, pending[0].BlobHash,
		"retained historical versions must not be queued for ordinary search")

	var searchable []string
	rows, err := target.db.QueryContext(ctx,
		`SELECT version_id FROM text_searchable_versions ORDER BY version_id`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var versionID string
		require.NoError(t, rows.Scan(&versionID))
		searchable = append(searchable, versionID)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{current.CurrentVersionID}, searchable)
}

func TestImportMetadataRejectsInvalidProvenanceAuthorityAndRollsBack(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	prior := firstProvenanceRecord(t, exported.Bytes())

	dangling := strings.Repeat("d", 64)
	crossNode := metadataProvenance{
		Type: metadataProvenanceType, NodeID: 11, IngestID: prior.IngestID,
		OriginalPath: "/source/cross-node.txt", Supersedes: &prior.Identity,
	}
	crossNode.Identity, err = provenanceIdentity(crossNode)
	require.NoError(t, err)
	firstSuccessor := metadataProvenance{
		Type: metadataProvenanceType, NodeID: prior.NodeID, IngestID: prior.IngestID,
		OriginalPath: "/source/corrected-one.txt", Supersedes: &prior.Identity,
	}
	firstSuccessor.Identity, err = provenanceIdentity(firstSuccessor)
	require.NoError(t, err)
	secondSuccessor := metadataProvenance{
		Type: metadataProvenanceType, NodeID: prior.NodeID, IngestID: prior.IngestID,
		OriginalPath: "/source/corrected-two.txt", Supersedes: &prior.Identity,
	}
	secondSuccessor.Identity, err = provenanceIdentity(secondSuccessor)
	require.NoError(t, err)

	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name: "identity mismatch",
			input: mutateFirstProvenanceRecord(t, exported.Bytes(), false, func(record *metadataProvenance) {
				record.OriginalPath = "/source/altered.txt"
			}),
			want: "provenance identity does not match",
		},
		{
			name: "dangling predecessor",
			input: mutateFirstProvenanceRecord(t, exported.Bytes(), true, func(record *metadataProvenance) {
				record.Supersedes = &dangling
			}),
			want: "provenance supersedes a missing fact",
		},
		{
			name:  "cross-node predecessor",
			input: appendMetadataRecords(t, exported.Bytes(), crossNode),
			want:  "provenance supersession must stay on one node",
		},
		{
			name:  "two direct successors",
			input: appendMetadataRecords(t, exported.Bytes(), firstSuccessor, secondSuccessor),
			want:  "UNIQUE constraint failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, openErr := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, openErr)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			targetVaultID := target.VaultID()
			importErr := target.ImportMetadata(t.Context(), bytes.NewReader(test.input))
			require.ErrorContains(t, importErr, test.want)
			assert.Equal(t, targetVaultID, target.VaultID())
			var nodes, provenanceRows int64
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM provenance`).Scan(&provenanceRows))
			assert.Equal(t, int64(1), nodes)
			assert.Zero(t, provenanceRows)
		})
	}
}

func TestImportMetadataRejectsWatchSourceRetargetedToDirectoryAndRollsBack(t *testing.T) {
	ctx := t.Context()
	source := newTestStore(t)
	run, err := source.BeginIngest(ctx, "watch", "sessions")
	require.NoError(t, err)
	_, err = source.IngestFileExact(
		ctx, run, source.RootID(), "session.jsonl", metadataHashCurrent, 12,
		"application/json", "daily/session.jsonl", "",
	)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))

	lines := bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte{'\n'})
	for index, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		switch kind.Type {
		case metadataWatchSourceType:
			var record metadataWatchSource
			require.NoError(t, json.Unmarshal(line, &record))
			record.NodeID = source.RootID()
			lines[index], err = json.Marshal(record)
			require.NoError(t, err)
		case metadataProvenanceType:
			var record metadataProvenance
			require.NoError(t, json.Unmarshal(line, &record))
			record.NodeID = source.RootID()
			record.Identity, err = provenanceIdentity(record)
			require.NoError(t, err)
			lines[index], err = json.Marshal(record)
			require.NoError(t, err)
		}
	}

	target := newTestStore(t)
	err = target.ImportMetadata(ctx, bytes.NewReader(append(bytes.Join(lines, []byte{'\n'}), '\n')))
	require.ErrorContains(t, err, "references non-file node")
	var nodes, cursors int64
	require.NoError(t, target.db.QueryRow(`
		SELECT (SELECT COUNT(*) FROM nodes), (SELECT COUNT(*) FROM watch_sources)`,
	).Scan(&nodes, &cursors))
	assert.Equal(t, int64(1), nodes)
	assert.Zero(t, cursors)
}

func TestImportMetadataRejectsWatchSourcesSharingNodeAndRollsBack(t *testing.T) {
	ctx := t.Context()
	source := newTestStore(t)
	run, err := source.BeginIngest(ctx, "watch", "sessions")
	require.NoError(t, err)
	node, err := source.IngestFileExact(
		ctx, run, source.RootID(), "session.jsonl", metadataHashCurrent, 12,
		"application/json", "daily/session.jsonl", "",
	)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))

	err = requireWatchSourceFiles("cursor", map[watchSourceKey]int64{
		{watchName: "sessions", sourceRef: "daily/session.jsonl"}: node.ID,
		{watchName: "other", sourceRef: "other.jsonl"}:            node.ID,
	}, map[int64]string{node.ID: nodeKindFile})
	require.ErrorContains(t, err, "reference the same node")

	var malformed bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSpace(exported.Bytes()), []byte{'\n'}) {
		malformed.Write(line)
		malformed.WriteByte('\n')
		var record metadataWatchSource
		if err := json.Unmarshal(line, &record); err != nil || record.Type != metadataWatchSourceType {
			continue
		}
		record.WatchName = "other"
		record.SourceRef = "other.jsonl"
		encoded, marshalErr := json.Marshal(record)
		require.NoError(t, marshalErr)
		malformed.Write(encoded)
		malformed.WriteByte('\n')
	}

	target := newTestStore(t)
	err = target.ImportMetadata(ctx, bytes.NewReader(malformed.Bytes()))
	require.Error(t, err)
	var nodes, cursors int64
	require.NoError(t, target.db.QueryRow(`
		SELECT (SELECT COUNT(*) FROM nodes), (SELECT COUNT(*) FROM watch_sources)`,
	).Scan(&nodes, &cursors))
	assert.Equal(t, int64(1), nodes)
	assert.Zero(t, cursors)
}

func TestMetadataRelationsRejectProvenanceCycle(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	firstIdentity := strings.Repeat("d", 64)
	secondIdentity := strings.Repeat("e", 64)
	_, err = source.db.Exec(`INSERT INTO provenance(
		identity,node_id,ingest_id,original_path,original_mtime,supersedes
	) VALUES
		(?,10,?,'/cycle/one',NULL,?),
		(?,10,?,'/cycle/two',NULL,?)`,
		firstIdentity, metadataIngestID, secondIdentity,
		secondIdentity, metadataIngestID, firstIdentity)
	require.NoError(t, err)
	require.ErrorContains(t,
		validateMetadataRelations(t.Context(), source.db),
		"provenance supersession graph contains a cycle",
	)
}

func TestImportMetadataRejectsDanglingContentAndRollsBack(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	targetVaultID := target.VaultID()

	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":2}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"node","id":2,"parent_id":1,"name":"missing.bin","kind":"file","current_version_id":"44444444-4444-4444-8444-444444444444","revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
	}, "\n") + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "current version does not belong")
	assert.Equal(t, int64(1), target.RootID())
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes, "failed import must leave the pristine target intact")
	assert.Equal(t, targetVaultID, target.VaultID())
	var storedVaultID string
	require.NoError(t, target.db.QueryRow(
		`SELECT vault_uid FROM vault_metadata WHERE singleton = 1`,
	).Scan(&storedVaultID))
	assert.Equal(t, targetVaultID, storedVaultID)
}

func TestImportMetadataRejectsInvalidUTF8AndRollsBack(t *testing.T) {
	const (
		header = `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
		root   = `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}` + "\n"
	)
	withInvalidByte := func(prefix, suffix string) []byte {
		result := append([]byte(prefix), 0xff)
		return append(result, suffix...)
	}
	tests := map[string]struct {
		input []byte
	}{
		"header string": {
			input: withInvalidByte(
				`{"type":"meta","format":"docbank-`, `","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}`+"\n"),
		},
		"record string": {
			input: withInvalidByte(
				header+root+`{"type":"tag","tag_id":"`+metadataTagID+`","name":"invalid`, `","revision":1}`+"\n"),
		},
		"lone high surrogate": {
			input: []byte(header + root + `{"type":"tag","tag_id":"` + metadataTagID + `","name":"invalid\ud800","revision":1}` + "\n"),
		},
		"lone low surrogate": {
			input: []byte(header + root + `{"type":"tag","tag_id":"` + metadataTagID + `","name":"invalid\udc00","revision":1}` + "\n"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			err = target.ImportMetadata(t.Context(), bytes.NewReader(tt.input))
			require.Error(t, err)
			var nodes, tags int64
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tags))
			assert.Equal(t, int64(1), nodes)
			assert.Zero(t, tags)
		})
	}
}

func TestImportMetadataAcceptsValidSurrogatePair(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"tag","tag_id":"` + metadataTagID + `","name":"archive \ud83d\ude00","revision":1}`,
	}, "\n") + "\n"
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(t.Context(), strings.NewReader(input)))
	var name string
	require.NoError(t, target.db.QueryRow(`SELECT name FROM tags WHERE id=?`, metadataTagID).Scan(&name))
	assert.Equal(t, "archive 😀", name)
}

func TestImportMetadataRejectsLaterContentCreateAndRollsBack(t *testing.T) {
	ctx := t.Context()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	malformed := strings.Replace(exported.String(),
		`"transition_kind":"content_replace"`, `"transition_kind":"content_create"`, 1)
	require.NotEqual(t, exported.String(), malformed)

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	err = target.ImportMetadata(ctx, strings.NewReader(malformed))
	require.ErrorContains(t, err, "content_create is required exactly at node revision one")
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes, "failed import must leave the pristine target intact")
}

func TestImportMetadataRejectsEmptyNonNullContentMIME(t *testing.T) {
	ctx := t.Context()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	malformed := strings.Replace(
		exported.String(), `"mime_type":"text/plain"`, `"mime_type":""`, 1,
	)
	require.NotEqual(t, exported.String(), malformed)

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	err = target.ImportMetadata(ctx, strings.NewReader(malformed))
	require.ErrorContains(t, err, "content version mime_type must be null or non-empty")
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes, "failed import must leave the pristine target intact")
}

func TestImportMetadataRejectsInvalidContentRelationshipsAndRollsBack(t *testing.T) {
	ctx := t.Context()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))

	tests := []struct {
		name    string
		old     string
		replace string
		want    string
	}{
		{
			name: "revert source names different content",
			old:  `"transition_kind":"content_replace","source_version_id":null`,
			replace: `"transition_kind":"content_revert","source_version_id":"` +
				metadataVersionOld + `"`,
			want: "revert source content differs from new version",
		},
		{
			name:    "create time differs from node creation",
			old:     `"recorded_at":"2026-01-08T00:00:00.000000000Z","node_revision":1`,
			replace: `"recorded_at":"2026-01-08T00:00:01.000000000Z","node_revision":1`,
			want:    "content_create time differs from node creation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			malformed := strings.Replace(exported.String(), tt.old, tt.replace, 1)
			require.NotEqual(t, exported.String(), malformed)
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			err = target.ImportMetadata(ctx, strings.NewReader(malformed))
			require.ErrorContains(t, err, tt.want)
			var nodes int64
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
			assert.Equal(t, int64(1), nodes, "failed import must leave the pristine target intact")
		})
	}
}

func TestImportMetadataRejectsEachRevertContentMismatch(t *testing.T) {
	ctx := t.Context()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	const alternateHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	tests := []struct {
		name   string
		mutate func(current, source map[string]any, records *[]map[string]any)
	}{
		{
			name: "hash only",
			mutate: func(current, _ map[string]any, records *[]map[string]any) {
				current["blob_hash"] = alternateHash
				current["size"] = float64(9)
				*records = append(*records, map[string]any{
					"type": "blob", "hash": alternateHash, "size": float64(9),
					"created_at": "2026-01-16T00:00:00.000000000Z",
				})
			},
		},
		{
			name: "size only",
			mutate: func(current, _ map[string]any, _ *[]map[string]any) {
				current["blob_hash"] = metadataHashVersion
			},
		},
		{
			name: "null source MIME",
			mutate: func(current, source map[string]any, _ *[]map[string]any) {
				current["blob_hash"] = metadataHashVersion
				current["size"] = float64(9)
				source["mime_type"] = nil
			},
		},
		{
			name: "null new MIME",
			mutate: func(current, _ map[string]any, _ *[]map[string]any) {
				current["blob_hash"] = metadataHashVersion
				current["size"] = float64(9)
				current["mime_type"] = nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var records []map[string]any
			for line := range bytes.SplitSeq(bytes.TrimSpace(exported.Bytes()), []byte{'\n'}) {
				var record map[string]any
				require.NoError(t, json.Unmarshal(line, &record))
				records = append(records, record)
			}
			var current, revertSource map[string]any
			for _, record := range records {
				switch record["version_id"] {
				case metadataVersionCurrent:
					current = record
				case metadataVersionOld:
					revertSource = record
				}
			}
			require.NotNil(t, current)
			require.NotNil(t, revertSource)
			current["transition_kind"] = "content_revert"
			current["source_version_id"] = metadataVersionOld
			tt.mutate(current, revertSource, &records)

			var malformed bytes.Buffer
			enc := jsontext.NewEncoder(&malformed)
			for _, record := range records {
				require.NoError(t, json.MarshalEncode(enc, record))
			}
			target, openErr := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, openErr)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			importErr := target.ImportMetadata(ctx, &malformed)
			require.ErrorContains(t, importErr, "revert source content differs from new version")
			var nodes int64
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
			assert.Equal(t, int64(1), nodes)
		})
	}
}

func TestExportMetadataRejectsMalformedContentVersion(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      string
	}{
		{
			name:      "operation UUID",
			statement: `UPDATE content_versions SET introduced_operation_id='not-a-uuid' WHERE version_id=?`,
			want:      "invalid content version operation ID",
		},
		{
			name:      "recorded timestamp",
			statement: `UPDATE content_versions SET recorded_at='not-a-timestamp' WHERE version_id=?`,
			want:      "invalid content version recorded_at",
		},
		{
			name:      "MIME UTF-8",
			statement: `UPDATE content_versions SET mime_type=CAST(X'ff' AS TEXT) WHERE version_id=?`,
			want:      "content version mime_type: not valid UTF-8",
		},
		{
			name:      "empty non-null MIME",
			statement: `UPDATE content_versions SET mime_type='' WHERE version_id=?`,
			want:      "content version mime_type must be null or non-empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := Open(filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			seedMetadataRoundTrip(t, source)
			_, err = source.db.Exec(tt.statement, metadataVersionCurrent)
			require.NoError(t, err)
			var exported bytes.Buffer
			err = source.ExportMetadata(t.Context(), &exported)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestExportMetadataRejectsForeignKeyCorruption(t *testing.T) {
	missingHash := strings.Repeat("d", 64)
	tests := []struct {
		name      string
		statement string
	}{
		{
			name: "content version node",
			statement: `INSERT INTO content_versions(
				version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
				introduced_operation_id,transition_kind,source_version_id
			) VALUES(
				'55555555-5555-4555-8555-555555555555',999,'` + metadataHashCurrent + `',12,
				'text/plain','2026-01-16T00:00:00.000000000Z',1,
				'66666666-6666-4666-8666-666666666666','content_create',NULL)`,
		},
		{
			name:      "content version blob",
			statement: `UPDATE content_versions SET blob_hash='` + missingHash + `' WHERE version_id='` + metadataVersionOld + `'`,
		},
		{
			name: "revert source version",
			statement: `UPDATE content_versions
				SET transition_kind='content_revert', source_version_id='77777777-7777-4777-8777-777777777777'
				WHERE version_id='` + metadataVersionCurrent + `'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := Open(filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			seedMetadataRoundTrip(t, source)
			conn, err := source.db.Conn(t.Context())
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), tt.statement)
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			var exported bytes.Buffer
			err = source.ExportMetadata(t.Context(), &exported)
			require.ErrorContains(t, err, "metadata violates foreign key")
		})
	}
}

func TestExportMetadataRejectsMissingRoot(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	conn, err := source.db.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM nodes`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	var exported bytes.Buffer
	err = source.ExportMetadata(t.Context(), &exported)
	require.ErrorContains(t, err, "tree does not have exactly one root")
}

func TestExportMetadataRejectsRegressedNodeSequence(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	_, err = source.db.Exec(`UPDATE sqlite_sequence SET seq=1 WHERE name='nodes'`)
	require.NoError(t, err)

	var exported bytes.Buffer
	err = source.ExportMetadata(t.Context(), &exported)
	require.ErrorContains(t, err, "below maximum node ID")
}

func TestImportMetadataRejectsOrphanedExtractionAndRollsBack(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"extracted_text","blob_hash":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","extractor":"plain","extractor_version":1,"status":"ok","error":null,"attempts":1,"text":"orphan","extracted_at":"2026-01-01T00:00:00.000000000Z"}`,
	}, "\n") + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "extracted text references missing blob authority")
	var nodes, extracted int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM extracted_text`).Scan(&extracted))
	assert.Equal(t, int64(1), nodes)
	assert.Zero(t, extracted)
}

func TestImportMetadataRejectsDisconnectedCycle(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	cyclic := strings.Replace(exported.String(),
		`"id":7,"parent_id":1`, `"id":7,"parent_id":12`, 1)
	cyclic = strings.Replace(cyclic,
		`"id":12,"parent_id":1`, `"id":12,"parent_id":7`, 1)

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	err = target.ImportMetadata(ctx, strings.NewReader(cyclic))
	require.ErrorContains(t, err, "unreachable")
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes)
}

func TestImportMetadataRejectsUnsafeTrashTopology(t *testing.T) {
	stamp := "2026-01-01T00:00:00.000000000Z"
	otherStamp := "2026-01-02T00:00:00.000000000Z"
	root := `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":null,"trash_parent":null,"trash_name":null}`
	tests := []struct {
		name    string
		records []string
		want    string
	}{
		{
			name: "restore parent inside subtree",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":3,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":2,"name":"B","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":null,"trash_name":null}`,
			},
			want: "trash parent points inside its subtree",
		},
		{
			name: "trash origin cycle",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":3,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":1,"name":"B","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":2,"trash_name":"B"}`,
			},
			want: "trash-origin topology contains a cycle",
		},
		{
			name: "trash root not detached",
			records: []string{
				`{"type":"node","id":3,"parent_id":1,"name":"container","kind":"dir","current_version_id":null,"revision":1,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":null,"trash_parent":null,"trash_name":null}`,
				`{"type":"node","id":2,"parent_id":3,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":1,"trash_name":"A"}`,
			},
			want: "trash root is not detached",
		},
		{
			name: "trashed node without trash root",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"orphan","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":null,"trash_name":null}`,
			},
			want: "does not belong to exactly one trash root",
		},
		{
			name: "trash descendant timestamp differs",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":1,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":2,"name":"B","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + otherStamp + `","trash_parent":null,"trash_name":null}`,
			},
			want: "live node or mismatched timestamp",
		},
		{
			name: "live descendant beneath trash root",
			records: []string{
				`{"type":"node","id":2,"parent_id":1,"name":"A","kind":"dir","current_version_id":null,"revision":2,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":"` + stamp + `","trash_parent":1,"trash_name":"A"}`,
				`{"type":"node","id":3,"parent_id":2,"name":"B","kind":"dir","current_version_id":null,"revision":1,"created_at":"` + stamp + `","modified_at":"` + stamp + `","trashed_at":null,"trash_parent":null,"trash_name":null}`,
			},
			want: "live node or mismatched timestamp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			lines := append([]string{
				`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":3}`,
				root,
			}, tt.records...)
			err = target.ImportMetadata(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"))
			require.ErrorContains(t, err, tt.want)
			var nodes int64
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
			assert.Equal(t, int64(1), nodes)
		})
	}
}

func TestImportMetadataRejectsNodeSequenceBelowSurvivingIDs(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	badSequence := strings.Replace(exported.String(), `"node_sequence":50`, `"node_sequence":10`, 1)

	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	err = target.ImportMetadata(ctx, strings.NewReader(badSequence))
	require.ErrorContains(t, err, "below maximum node ID")
	var nodes int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	assert.Equal(t, int64(1), nodes)
}

func TestImportMetadataRejectsNonPristineTarget(t *testing.T) {
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	_, err = target.Mkdir(context.Background(), target.RootID(), "existing")
	require.NoError(t, err)

	input := `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
	err = target.ImportMetadata(context.Background(), strings.NewReader(input))
	require.ErrorContains(t, err, "not pristine")
	_, err = target.NodeByPath(context.Background(), "/existing")
	require.NoError(t, err)
}

func TestImportMetadataRejectsOperationalRestoreState(t *testing.T) {
	const (
		header = `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
		blob   = `{"type":"blob","hash":"` + metadataHashCurrent + `","size":12,"created_at":"2026-01-01T00:00:00.000000000Z"}` + "\n"
		root   = `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}` + "\n"
	)
	tests := []struct {
		name       string
		insert     string
		countQuery string
		input      string
	}{
		{
			name: "rendition blob staging",
			insert: `INSERT INTO rendition_blob_staging(blob_hash)
				VALUES('` + metadataHashCurrent + `')`,
			countQuery: `SELECT COUNT(*) FROM rendition_blob_staging`,
			input:      header + blob + root,
		},
		{
			name: "derivative blob purge",
			insert: `INSERT INTO derivative_blob_purge_pending(blob_hash)
				VALUES('` + metadataHashCurrent + `')`,
			countQuery: `SELECT COUNT(*) FROM derivative_blob_purge_pending`,
			input:      header + blob + root,
		},
		{
			name:       "derivative pack purge",
			insert:     `INSERT INTO derivative_pack_purge_pending(store_id,pack_id) VALUES('stale-store','stale-pack')`,
			countQuery: `SELECT COUNT(*) FROM derivative_pack_purge_pending`,
			input:      header + root,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newTestStore(t)
			conn, err := target.db.Conn(t.Context())
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), test.insert)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			err = target.ImportMetadataForBackupRestore(t.Context(), strings.NewReader(test.input))
			require.ErrorContains(t, err, "not pristine")
			var rows int
			require.NoError(t, target.db.QueryRow(test.countQuery).Scan(&rows))
			assert.Equal(t, 1, rows, "rejected restore must preserve the target for inspection")
		})
	}
}

func TestImportMetadataRejectsLexicalProjectionState(t *testing.T) {
	target := newTestStore(t)
	generation, err := target.StageLexicalGeneration(t.Context(), fakeHash("cf"))
	require.NoError(t, err)
	_, err = target.db.ExecContext(t.Context(),
		`INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)`, generation.ID)
	require.NoError(t, err)

	input := `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
	err = target.ImportMetadata(t.Context(), strings.NewReader(input))
	require.ErrorContains(t, err, "not pristine")
	active, err := target.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, generation.ID, active.ID)
}

func TestImportMetadataRejectsUnknownVersionAndFields(t *testing.T) {
	for _, input := range []string{
		`{"type":"meta","format":"docbank-metadata","version":2,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"not-a-uuid","node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":null,"node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","vault_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1,"surprise":true}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n" +
			`{"type":"future_record","value":1}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":12}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":null,"created_at":"2026-01-01T00:00:00.000000000Z"}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","Size":12,"created_at":"2026-01-01T00:00:00.000000000Z"}` + "\n",
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n" +
			`{"type":"blob","hash":"` + metadataHashCurrent + `","size":12,"created_at":"2026-01-01T00:00:00.000000000+00:00"}` + "\n",
	} {
		t.Run(input, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			require.Error(t, target.ImportMetadata(context.Background(), strings.NewReader(input)))
		})
	}
}

func TestExportMetadataRejectsMalformedVaultIdentity(t *testing.T) {
	source := newTestStore(t)
	_, err := source.db.Exec(`UPDATE vault_metadata SET vault_uid = 'not-a-uuid' WHERE singleton = 1`)
	require.NoError(t, err)

	var exported bytes.Buffer
	err = source.ExportMetadata(t.Context(), &exported)
	require.ErrorContains(t, err, "invalid vault identity")
	assert.Empty(t, exported.Bytes())
}

func TestMetadataRejectsMalformedStableRecordIDs(t *testing.T) {
	t.Run("import", func(t *testing.T) {
		header := `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
		root := `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}` + "\n"
		for name, record := range map[string]string{
			"ingest": `{"type":"ingest","ingest_id":"not-a-uuid","started_at":"2026-01-01T00:00:00.000000000Z","source_kind":"cli","source_desc":"source"}`,
			"tag":    `{"type":"tag","tag_id":"not-a-uuid","name":"archive","revision":1}`,
		} {
			t.Run(name, func(t *testing.T) {
				target, err := Open(filepath.Join(t.TempDir(), "target.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, target.Close()) })
				err = target.ImportMetadata(t.Context(), strings.NewReader(header+root+record+"\n"))
				require.ErrorContains(t, err, "canonical UUIDv4")
			})
		}
	})

	t.Run("export", func(t *testing.T) {
		for name, insert := range map[string]string{
			"ingest": `INSERT INTO ingests(id,started_at,source_kind,source_desc)
				VALUES('not-a-uuid','2026-01-01T00:00:00.000000000Z','cli','source')`,
			"tag": `INSERT INTO tags(id,name) VALUES('not-a-uuid','archive')`,
		} {
			t.Run(name, func(t *testing.T) {
				source := newTestStore(t)
				_, err := source.db.Exec(insert)
				require.NoError(t, err)
				var exported bytes.Buffer
				err = source.ExportMetadata(t.Context(), &exported)
				require.ErrorContains(t, err, "canonical UUIDv4")
			})
		}
	})
}

func TestMetadataRejectsNonCanonicalTagNames(t *testing.T) {
	const (
		header = `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
		root   = `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}` + "\n"
	)
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	record := `{"type":"tag","tag_id":"` + metadataTagID + `","name":"cafe\u0301","revision":1}` + "\n"
	err = target.ImportMetadata(t.Context(), strings.NewReader(header+root+record))
	require.ErrorContains(t, err, "canonical NFC")

	var tags int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tags))
	assert.Zero(t, tags, "failed import must not retain a non-canonical tag")
}

func TestImportMetadataRejectsInvalidTagRevision(t *testing.T) {
	const (
		header = `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
		root   = `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}` + "\n"
	)
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	record := `{"type":"tag","tag_id":"` + metadataTagID + `","name":"archive","revision":0}` + "\n"
	err = target.ImportMetadata(t.Context(), strings.NewReader(header+root+record))
	require.ErrorContains(t, err, "invalid tag record")

	var tags int64
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tags))
	assert.Zero(t, tags)
}

func TestImportMetadataTreatsTagRevisionAsOpaque(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}`,
		`{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}`,
		`{"type":"tag","tag_id":"` + metadataTagID + `","name":"archive","revision":1}`,
		`{"type":"node_tag","node_id":1,"tag_id":"` + metadataTagID + `"}`,
	}, "\n") + "\n"
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	require.NoError(t, target.ImportMetadata(t.Context(), strings.NewReader(input)))
	tag, err := target.TagByID(t.Context(), metadataTagID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), tag.Revision)
	assert.Equal(t, 1, tag.AssignmentCount)
}

func TestImportMetadataRejectsDuplicateStableRecordIDsTransactionally(t *testing.T) {
	header := `{"type":"meta","format":"docbank-metadata","version":1,"vault_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","node_sequence":1}` + "\n"
	root := `{"type":"node","id":1,"parent_id":null,"name":"","kind":"dir","current_version_id":null,"revision":1,"created_at":"2026-01-01T00:00:00.000000000Z","modified_at":"2026-01-01T00:00:00.000000000Z","trashed_at":null,"trash_parent":null,"trash_name":null}` + "\n"
	for name, record := range map[string]string{
		"ingest": `{"type":"ingest","ingest_id":"` + metadataIngestID + `","started_at":"2026-01-01T00:00:00.000000000Z","source_kind":"cli","source_desc":"source"}`,
		"tag":    `{"type":"tag","tag_id":"` + metadataTagID + `","name":"archive","revision":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })

			err = target.ImportMetadata(t.Context(), strings.NewReader(header+root+record+"\n"+record+"\n"))
			require.Error(t, err)

			var recordCount int64
			require.NoError(t, target.db.QueryRow(
				`SELECT (SELECT COUNT(*) FROM ingests) + (SELECT COUNT(*) FROM tags)`,
			).Scan(&recordCount))
			assert.Zero(t, recordCount)
		})
	}
}

func seedMetadataRoundTrip(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	originalMTime := "2025-12-31T23:00:00.12Z"
	provenanceID, err := provenanceIdentity(metadataProvenance{
		Type: metadataProvenanceType, NodeID: 10, IngestID: metadataIngestID,
		OriginalPath: "/source/report.txt", OriginalMTime: &originalMTime,
	})
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`UPDATE nodes SET created_at='2026-01-01T00:00:00.000000000Z', modified_at='2026-01-02T00:00:00.000000000Z' WHERE id=1`,
			`INSERT INTO blobs(hash,size,created_at) VALUES
			 ('` + metadataHashCurrent + `',12,'2026-01-03T00:00:00.000000000Z'),
			 ('` + metadataHashTrashed + `',5,'2026-01-04T00:00:00.000000000Z'),
			 ('` + metadataHashVersion + `',9,'2026-01-05T00:00:00.000000000Z')`,
			`INSERT INTO blob_locations(
				blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
			)
			SELECT '` + metadataHashTrashed + `',store_id,
			       '40000000-0000-4000-8000-000000000001','loose','raw',5,1
			FROM blob_stores WHERE role='primary'
			UNION ALL
			SELECT '` + metadataHashVersion + `',store_id,
			       '40000000-0000-4000-8000-000000000002','loose','raw',9,1
			FROM blob_stores WHERE role='primary'`,
			`INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at) VALUES
			 (7,1,'Projects','dir','2026-01-06T00:00:00.000000000Z','2026-01-07T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at) VALUES
			 (12,1,'Empty','dir','2026-01-06T00:00:00.000000000Z','2026-01-07T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at)
			 VALUES(10,7,'report.txt','file','` + metadataVersionCurrent + `',3,
			 '2026-01-08T00:00:00.000000000Z','2026-01-09T00:00:00.000000000Z')`,
			`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at,trashed_at,trash_parent,trash_name)
			 VALUES(11,1,'old.bin','file','` + metadataVersionTrashed + `',2,
			 '2026-01-10T00:00:00.000000000Z','2026-01-11T00:00:00.000000000Z',
			 '2026-01-12T00:00:00.000000000Z',7,'old.bin')`,
			`INSERT INTO content_versions(
				version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
				introduced_operation_id,transition_kind,source_version_id
			) VALUES
			 ('` + metadataVersionOld + `',10,'` + metadataHashVersion + `',9,'text/plain',
			  '2026-01-08T00:00:00.000000000Z',1,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','content_create',NULL),
			 ('` + metadataVersionCurrent + `',10,'` + metadataHashCurrent + `',12,'text/plain',
			  '2026-01-09T00:00:00.000000000Z',2,'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','content_replace',NULL),
			 ('` + metadataVersionTrashed + `',11,'` + metadataHashTrashed + `',5,'application/octet-stream',
			  '2026-01-10T00:00:00.000000000Z',1,'cccccccc-cccc-4ccc-8ccc-cccccccccccc','content_create',NULL)`,
			`INSERT INTO ingests(id,started_at,source_kind,source_desc)
				 VALUES('` + metadataIngestID + `','2026-01-08T00:00:00.000000000Z','filesystem','dropbox')`,
			`INSERT INTO provenance(identity,node_id,ingest_id,original_path,original_mtime,supersedes)
				 VALUES('` + provenanceID + `',10,'` + metadataIngestID + `','/source/report.txt','` + originalMTime + `',NULL)`,
			`INSERT INTO tags(id,name,revision) VALUES('` + metadataTagID + `','important',7)`,
			`INSERT INTO node_tags(node_id,tag_id) VALUES(10,'` + metadataTagID + `')`,
			`INSERT INTO extracted_text(blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at)
			 VALUES('` + metadataHashCurrent + `','plain',2,'ok',NULL,1,'line one\nline two','2026-01-13T00:00:00.000000000Z')`,
			`INSERT INTO blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at)
			 SELECT store_id,'metadata-pack',1,12,'2026-01-14T00:00:00.000000000Z'
			 FROM blob_stores WHERE role='primary'`,
			`INSERT INTO blob_locations(
				blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
			)
			SELECT '` + metadataHashCurrent + `',store_id,
			       '40000000-0000-4000-8000-000000000003','packed',NULL,12,1
			FROM blob_stores WHERE role='primary'`,
			`INSERT INTO blob_pack_entries(
				blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
			)
			SELECT '` + metadataHashCurrent + `',store_id,'metadata-pack',16,12,12,0,42
			FROM blob_stores WHERE role='primary'`,
			`INSERT INTO nodes(id,parent_id,name,kind,created_at,modified_at)
			 VALUES(50,1,'historical-high-water','dir','2026-01-15T00:00:00.000000000Z','2026-01-15T00:00:00.000000000Z')`,
			`DELETE FROM nodes WHERE id=50`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}))
}

func firstProvenanceRecord(t *testing.T, input []byte) metadataProvenance {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataProvenanceType {
			continue
		}
		var record metadataProvenance
		require.NoError(t, json.Unmarshal(line, &record))
		return record
	}
	require.FailNow(t, "metadata lacks provenance record")
	return metadataProvenance{}
}

func mutateFirstProvenanceRecord(
	t *testing.T,
	input []byte,
	recomputeIdentity bool,
	mutate func(*metadataProvenance),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataProvenanceType {
			continue
		}
		var record metadataProvenance
		require.NoError(t, json.Unmarshal(line, &record))
		mutate(&record)
		if recomputeIdentity {
			var err error
			record.Identity, err = provenanceIdentity(record)
			require.NoError(t, err)
		}
		var err error
		lines[index], err = json.Marshal(record)
		require.NoError(t, err)
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "metadata lacks provenance record")
	return nil
}

func appendMetadataRecords(t *testing.T, input []byte, records ...any) []byte {
	t.Helper()
	result := bytes.Clone(input)
	for _, record := range records {
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		result = append(result, encoded...)
		result = append(result, '\n')
	}
	return result
}

func TestProcessingMetadataJSONLIsDependencyOrderedAndCrossDriverStable(t *testing.T) {
	ctx := t.Context()
	source := newTestStore(t)
	versions, profiles, build := seedProcessingMetadataCatalog(t, source)

	var first, second bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &first))
	require.NoError(t, source.ExportMetadata(ctx, &second))
	assert.Equal(t, first.Bytes(), second.Bytes(), "unchanged processing authority must export byte-identically")
	assert.Equal(t, []string{
		"processing_profile", "processing_profile", "rendition_build",
		"rendition_artifact", "rendition_artifact", "rendition_unit",
		"rendition_lexical_segment", "rendition_attachment", "rendition_attachment",
		"rendition_head", "rendition_head",
	}, processingMetadataRecordTypes(t, first.Bytes()))
	assert.Contains(t, first.String(), `"blob_hash":"`+catalogEvidenceBlobHash+`"`,
		"derivative blob membership must be portable authority")
	assert.Contains(t, first.String(), `"blob_hash":"`+catalogMarkdownBlobHash+`"`)

	targetPath := filepath.Join(t.TempDir(), "modernc-target.db")
	materializeCatalogBlobs(t, targetPath, "", "")
	target, err := Open(targetPath, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))

	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, first.Bytes(), restored.Bytes(),
		"mattn/default and modernc must preserve the exact processing JSONL bytes")
	sourceView, err := source.ActiveRendition(ctx, versions[0], profiles[0].Fingerprint)
	require.NoError(t, err)
	restoredView, err := target.ActiveRendition(ctx, versions[0], profiles[0].Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, sourceView, restoredView)
	assert.Equal(t, build, restoredView.Build)
}

func TestProcessingMetadataRoundTripsHeadedEmptyLexicalGeneration(t *testing.T) {
	// Mutation caught: requiring a headed generation to contain builds omits the
	// sole lexical serving pointer from metadata backup and restore.
	source := newTestStore(t)
	generation, err := source.StageLexicalGeneration(t.Context(), fakeHash("ce"))
	require.NoError(t, err)
	require.Zero(t, generation.BuildCount)
	_, err = source.db.ExecContext(t.Context(),
		`INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)`, generation.ID)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	assert.Contains(t, exported.String(), `"generation_id":"`+generation.ID+`"`)

	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	restored, err := target.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, generation.ID, restored.ID)
	assert.Zero(t, restored.BuildCount)
}

func TestProcessingMetadataImportRequiresVerifiedPhysicalBytes(t *testing.T) {
	source := newTestStore(t)
	seedProcessingMetadataCatalog(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	for name, setup := range map[string]func(*testing.T, string){
		"metadata only": func(t *testing.T, _ string) {
			t.Helper()
		},
		"missing derivative": func(t *testing.T, targetPath string) {
			t.Helper()
			materializeCatalogBlobs(t, targetPath, catalogEvidenceBlobHash, "")
		},
		"corrupt derivative": func(t *testing.T, targetPath string) {
			t.Helper()
			materializeCatalogBlobs(t, targetPath, "", catalogMarkdownBlobHash)
		},
	} {
		t.Run(name, func(t *testing.T) {
			targetPath := filepath.Join(t.TempDir(), "target.db")
			setup(t, targetPath)
			target, err := Open(targetPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, target.Close()) })

			err = target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes()))
			require.ErrorContains(t, err, "physical")

			var processingRows, heads int
			require.NoError(t, target.db.QueryRow(`
				SELECT
				  (SELECT COUNT(*) FROM processing_profiles)
				    + (SELECT COUNT(*) FROM rendition_builds)
				    + (SELECT COUNT(*) FROM rendition_artifacts)
				    + (SELECT COUNT(*) FROM rendition_units)
				    + (SELECT COUNT(*) FROM rendition_lexical_segments)
				    + (SELECT COUNT(*) FROM rendition_attachments),
				  (SELECT COUNT(*) FROM rendition_heads)
			`).Scan(&processingRows, &heads))
			assert.Zero(t, processingRows)
			assert.Zero(t, heads)
		})
	}
}

func TestProcessingMetadataNilHeadingPathRoundTripsAsEmptyArray(t *testing.T) {
	source := newTestStore(t)
	versions := seedRenditionCatalogVersions(t, source)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(source, profile)
	build.Units[0].HeadingPath = nil
	require.NoError(t, source.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: source.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, publishRenditionForTest(
		t, source, attachment, "2026-08-22T10:01:00.000000000Z", fakeHash("c8"),
	))

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	assert.Contains(t, exported.String(), `"heading_path":[]`)
	assert.NotContains(t, exported.String(), `"heading_path":null`)

	targetPath := filepath.Join(t.TempDir(), "target.db")
	materializeCatalogBlobs(t, targetPath, "", "")
	target, err := Open(targetPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))

	view, err := target.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Empty(t, view.Build.Units[0].HeadingPath)
	assert.NotNil(t, view.Build.Units[0].HeadingPath)
}

func TestProcessingMetadataImportRejectsInvalidAuthorityTransactionally(t *testing.T) {
	source := newTestStore(t)
	seedProcessingMetadataCatalog(t, source)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	for name, mutate := range map[string]func(*testing.T, []byte) []byte{
		"duplicate immutable profile": func(t *testing.T, input []byte) []byte {
			t.Helper()
			line := firstProcessingMetadataRecord(t, input, "processing_profile")
			return append(append(bytes.Clone(input), line...), '\n')
		},
		"missing declared artifact": func(t *testing.T, input []byte) []byte {
			t.Helper()
			return removeFirstProcessingMetadataRecord(t, input, "rendition_artifact")
		},
		"missing attachment reference": func(t *testing.T, input []byte) []byte {
			t.Helper()
			return mutateFirstProcessingMetadataRecord(t, input, "rendition_head", func(fields map[string]jsontext.Value) {
				fields["attachment_id"] = jsontext.Value(`"` + fakeHash("fe") + `"`)
			})
		},
		"artifact checksum disagreement": func(t *testing.T, input []byte) []byte {
			t.Helper()
			return mutateFirstProcessingMetadataRecord(t, input, "rendition_artifact", func(fields map[string]jsontext.Value) {
				fields["checksum"] = jsontext.Value(`"` + fakeHash("2f") + `"`)
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			target := newTestStore(t)
			originalVaultID := target.VaultID()
			err := target.ImportMetadata(t.Context(), bytes.NewReader(mutate(t, exported.Bytes())))
			require.Error(t, err)

			var processingRows, nodes int
			require.NoError(t, target.db.QueryRow(`
				SELECT
				  (SELECT COUNT(*) FROM processing_profiles)
				    + (SELECT COUNT(*) FROM rendition_builds)
				    + (SELECT COUNT(*) FROM rendition_artifacts)
				    + (SELECT COUNT(*) FROM rendition_units)
				    + (SELECT COUNT(*) FROM rendition_lexical_segments)
				    + (SELECT COUNT(*) FROM rendition_attachments)
				    + (SELECT COUNT(*) FROM rendition_heads),
				  (SELECT COUNT(*) FROM nodes)
			`).Scan(&processingRows, &nodes))
			assert.Zero(t, processingRows, "a failed import must publish no processing rows or heads")
			assert.Equal(t, 1, nodes, "a failed import must restore the pristine root")
			assert.Equal(t, originalVaultID, target.VaultID(), "a failed import must not replace vault identity")
		})
	}
}

func seedProcessingMetadataCatalog(
	t *testing.T, s *Store,
) ([]string, []ProcessingProfileRecord, RenditionBuildRecord) {
	t.Helper()
	versions := seedRenditionCatalogVersions(t, s)
	profiles := []ProcessingProfileRecord{
		catalogProcessingProfile(t, false), catalogProcessingProfile(t, true),
	}
	build := catalogRenditionBuild(s, profiles[0])
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachments := []RenditionAttachmentRecord{
		{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
			BuildID: build.ID, Profile: profiles[0], AttachedAt: "2026-08-22T10:00:00.000000000Z"},
		{ID: catalogAttachmentSecond, VaultID: s.VaultID(), ContentVersionID: versions[1],
			BuildID: build.ID, Profile: profiles[1], AttachedAt: "2026-08-22T10:01:00.000000000Z"},
	}
	for index, attachment := range attachments {
		require.NoError(t, publishRenditionForTest(
			t, s, attachment,
			time.Date(2026, time.August, 22, 10, 2+index, 0, 0, time.UTC).Format(timestampLayout),
			fakeHash("c9"),
		))
	}
	return versions, profiles, build
}

func processingMetadataRecordTypes(t *testing.T, input []byte) []string {
	t.Helper()
	var result []string
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		switch kind.Type {
		case "processing_profile", "rendition_build", "rendition_artifact",
			"rendition_unit", "rendition_lexical_segment", "rendition_attachment", "rendition_head":
			result = append(result, kind.Type)
		}
	}
	return result
}

func firstProcessingMetadataRecord(t *testing.T, input []byte, kind string) []byte {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var recordKind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &recordKind))
		if recordKind.Type == kind {
			return bytes.Clone(line)
		}
	}
	require.FailNow(t, "processing metadata record not found", kind)
	return nil
}

func removeFirstProcessingMetadataRecord(t *testing.T, input []byte, kind string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var recordKind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &recordKind))
		if recordKind.Type == kind {
			lines = append(lines[:index], lines[index+1:]...)
			return append(bytes.Join(lines, []byte{'\n'}), '\n')
		}
	}
	require.FailNow(t, "processing metadata record not found", kind)
	return nil
}

func mutateFirstProcessingMetadataRecord(
	t *testing.T, input []byte, kind string, mutate func(map[string]jsontext.Value),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var recordKind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &recordKind))
		if recordKind.Type != kind {
			continue
		}
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(line, &fields))
		mutate(fields)
		var err error
		lines[index], err = json.Marshal(fields, json.Deterministic(true))
		require.NoError(t, err)
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "processing metadata record not found", kind)
	return nil
}

func materializeCatalogBlobs(t *testing.T, databasePath, omittedHash, corruptHash string) {
	t.Helper()
	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(databasePath), "blobs"), packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(t, err)
	loose, err := packstore.NewLooseStore(layout)
	require.NoError(t, err)
	for hash, content := range catalogBlobContents {
		if hash == omittedHash {
			continue
		}
		parsed, parseErr := packstore.ParseHash(hash)
		require.NoError(t, parseErr)
		_, writeErr := loose.WriteBytes(t.Context(), content, packstore.WriteOptions{
			Durability: packstore.AtomicPublication, Dedup: packstore.VerifyFullHash,
			ExpectedHash: parsed, ExpectedSize: int64(len(content)), SizeKnown: true,
		})
		require.NoError(t, writeErr)
		if hash == corruptHash {
			require.NoError(t, os.WriteFile(layout.LoosePath(parsed), bytes.Repeat([]byte{'x'}, len(content)), 0o600))
		}
	}
}
