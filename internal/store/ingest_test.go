package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestFileIdempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "/src/docs")
	require.NoError(t, err)

	// First import.
	n1, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 10, "application/pdf", "/src/docs/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report.pdf", n1.Name)

	// Same content, same name: skipped.
	skipped, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 10, "application/pdf", "/src/docs/report.pdf", "")
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, n1.ID, skipped.ID)

	// Different content, same name: suffixed.
	n2, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("b2"), 11, "application/pdf", "/other/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (2).pdf", n2.Name)

	// Re-run of the suffixed one: skipped even though names differ.
	skipped, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("b2"), 11, "application/pdf", "/other/report.pdf", "")
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, n2.ID, skipped.ID)
	physical, err := s.PhysicalContent(ctx, fakeHash("b2"))
	require.NoError(t, err)
	require.Equal(t, "raw", physical.Encoding)
	skipped, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("b2"), 11, "application/pdf", "/other/report.pdf", "",
		BlobPhysical{Encoding: "zstd", StoredBytes: 7, PackEligible: true, Created: true})
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, n2.ID, skipped.ID)
	physical, err = s.PhysicalContent(ctx, fakeHash("b2"))
	require.NoError(t, err)
	assert.Equal(t, "zstd", physical.Encoding)
	assert.Equal(t, int64(7), physical.StoredBytes)

	// Third distinct content takes the next free ordinal.
	n3, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("c3"), 12, "application/pdf", "/more/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (3).pdf", n3.Name)

	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE blobs SET loose_encoding=NULL, loose_stored_size=NULL WHERE hash=?`,
			fakeHash("a1"))
		return err
	}))
	_, _, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 10, "application/pdf", "/src/docs/report.pdf", "")
	require.ErrorIs(t, err, ErrPhysicalAuthorityMissing)
}

func TestFilesystemIngestDoesNotAdoptEmbeddedOpaqueReference(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	embedded, err := s.BeginEmbeddedIngest(ctx, "cli", "application archive")
	require.NoError(t, err)
	embeddedNode, err := s.IngestFileExact(
		ctx, embedded, s.RootID(), "archived.pdf", fakeHash("a1"), 10,
		"application/pdf", "objects/report.pdf", "",
	)
	require.NoError(t, err)

	filesystem, err := s.BeginIngest(ctx, "cli", "/source")
	require.NoError(t, err)
	imported, added, err := s.IngestFile(
		ctx, filesystem, s.RootID(), "report.pdf", fakeHash("a1"), 10,
		"application/pdf", "/source/report.pdf", "",
	)
	require.NoError(t, err)
	require.True(t, added)
	require.NotEqual(t, embeddedNode.ID, imported.ID)
	require.Equal(t, "report.pdf", imported.Name)
}

func TestEmbeddedWatchKindIsPortableProvenanceNotOperationalState(t *testing.T) {
	ctx := t.Context()
	source := newTestStore(t)
	run, err := source.BeginEmbeddedIngest(ctx, "watch", "application archive")
	require.NoError(t, err)
	node, err := source.IngestFileExact(
		ctx, run, source.RootID(), "record.jsonl", fakeHash("a1"), 10,
		"application/x-ndjson", "records/record.jsonl", "",
	)
	require.NoError(t, err)
	require.NoError(t, source.ValidateMetadata(ctx))

	page, err := source.NodeProvenance(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "watch", page.Items[0].SourceKind)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	require.Contains(t, exported.String(), `"source_kind":"embedded:watch"`)
	require.NotContains(t, exported.String(), `"type":"watch_source"`)

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	restoredPage, err := restored.NodeProvenance(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, restoredPage.Items, 1)
	require.Equal(t, "watch", restoredPage.Items[0].SourceKind)
}

func TestIngestFileMatchesAcrossSuffixGap(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)

	// Occupy report.pdf and report (3).pdf; (2) is free (e.g. it was trashed).
	_, _, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "p1", "")
	require.NoError(t, err)
	f2, err := s.CreateFile(ctx, s.RootID(), "report (3).pdf", fakeHash("c3"), 1, "application/pdf")
	require.NoError(t, err)
	_ = f2

	// Content matching the gapped candidate must still be skipped: the
	// resolver checks ALL live candidates, not just up to the first gap.
	_, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("c3"), 1, "application/pdf", "p2", "")
	require.NoError(t, err)
	assert.False(t, added)

	// New content fills the gap.
	n, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("d4"), 1, "application/pdf", "p3", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (2).pdf", n.Name)
}

func TestIngestFileRecordsProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(ing.ID()))
	n, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"a.txt", fakeHash("a1"), 1, "text/plain", "/orig/a.txt", "2026-01-02T03:04:05Z")
	require.NoError(t, err)
	require.True(t, added)

	var identity, origPath, origMtime string
	var supersedes sql.NullString
	require.NoError(t, s.db.QueryRow(
		`SELECT identity, original_path, original_mtime, supersedes
		 FROM provenance WHERE node_id = ?`,
		n.ID).Scan(&identity, &origPath, &origMtime, &supersedes))
	assert.Equal(t, "/orig/a.txt", origPath)
	assert.Equal(t, "2026-01-02T03:04:05Z", origMtime)
	assert.False(t, supersedes.Valid)
	var storedIngestID string
	require.NoError(t, s.db.QueryRow(
		`SELECT ingest_id FROM provenance WHERE node_id = ?`, n.ID,
	).Scan(&storedIngestID))
	assert.Equal(t, ing.ID(), storedIngestID)
	wantIdentity, err := provenanceIdentity(metadataProvenance{
		Type: metadataProvenanceType, NodeID: n.ID, IngestID: ing.ID(),
		OriginalPath: origPath, OriginalMTime: &origMtime,
	})
	require.NoError(t, err)
	assert.Equal(t, wantIdentity, identity)
}

func TestSyncWatchedContentFollowsMovedNodeWithoutOverwritingIndependentEdit(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "watch", "sessions")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(),
		"session.jsonl", fakeHash("a1"), 1, "application/json", "daily/session.jsonl", "")
	require.NoError(t, err)
	destination, err := s.MkdirAll(ctx, "/organized")
	require.NoError(t, err)
	moved, _, err := s.Move(
		ctx, node.ID, destination.ID, "renamed.jsonl", UnconditionalRev,
	)
	require.NoError(t, err)

	updated, version, changed, err := s.SyncWatchedContent(
		ctx, "sessions", "daily/session.jsonl",
		fakeHash("b2"), 2, "application/json",
	)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, moved.ID, updated.ID)
	assert.Equal(t, "renamed.jsonl", updated.Name)
	assert.Equal(t, moved.Revision+1, updated.Revision)
	assert.Equal(t, fakeHash("b2"), version.BlobHash)

	unchanged, noVersion, changed, err := s.SyncWatchedContent(
		ctx, "sessions", "daily/session.jsonl",
		fakeHash("b2"), 2, "application/json",
		BlobPhysical{Encoding: "zstd", StoredBytes: 1, PackEligible: true, Created: true},
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, updated, unchanged)
	assert.Empty(t, noVersion.ID)
	physical, err := s.PhysicalContent(ctx, fakeHash("b2"))
	require.NoError(t, err)
	assert.Equal(t, "zstd", physical.Encoding)

	manuallyEdited, _, err := s.ReplaceContent(
		ctx, updated.ID, updated.Revision, fakeHash("c3"), 3, "application/json",
	)
	require.NoError(t, err)
	unchanged, noVersion, changed, err = s.SyncWatchedContent(
		ctx, "sessions", "daily/session.jsonl",
		fakeHash("b2"), 2, "application/json",
		BlobPhysical{Encoding: "zstd", StoredBytes: 1, PackEligible: true},
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, manuallyEdited, unchanged)
	assert.Empty(t, noVersion.ID)

	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE blobs SET loose_encoding=NULL, loose_stored_size=NULL WHERE hash=?`,
			fakeHash("c3"))
		return err
	}))
	missingNode, missingVersion, missingChanged, err := s.SyncWatchedContent(
		ctx, "sessions", "daily/session.jsonl",
		fakeHash("b2"), 2, "application/json",
		BlobPhysical{Encoding: "zstd", StoredBytes: 1, PackEligible: true},
	)
	require.ErrorIs(t, err, ErrPhysicalAuthorityMissing)
	assert.Empty(t, missingNode.Kind)
	assert.Empty(t, missingVersion.ID)
	assert.False(t, missingChanged)
}

func TestWatchSourceLookupUsesPrimaryKeyAtArchiveScale(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "watch", "sessions")
	require.NoError(t, err)
	_, err = s.IngestFileExact(ctx, run, s.RootID(),
		"session.jsonl", fakeHash("a1"), 1, "application/json", "daily/session.jsonl", "")
	require.NoError(t, err)

	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	nodeStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO nodes(parent_id,name,kind,current_version_id,created_at,modified_at)
		VALUES(?,?,'file',?,?,?)`)
	require.NoError(t, err)
	defer func() { require.NoError(t, nodeStmt.Close()) }()
	versionStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO content_versions(
			version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
			introduced_operation_id,transition_kind,source_version_id
		) VALUES(?,?,?,?,?,?,1,?,'content_create',NULL)`)
	require.NoError(t, err)
	defer func() { require.NoError(t, versionStmt.Close()) }()
	watchStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO watch_sources(watch_name,source_ref,node_id,blob_hash,size)
		VALUES(?,?,?,?,?)`)
	require.NoError(t, err)
	defer func() { require.NoError(t, watchStmt.Close()) }()
	stamp := nowRFC3339()
	for i := range 10_000 {
		versionID, idErr := newUUIDv4()
		require.NoError(t, idErr)
		operationID, idErr := newUUIDv4()
		require.NoError(t, idErr)
		name := fmt.Sprintf("file-%05d", i)
		result, execErr := nodeStmt.ExecContext(
			ctx, s.RootID(), name, versionID, stamp, stamp,
		)
		require.NoError(t, execErr)
		nodeID, idErr := result.LastInsertId()
		require.NoError(t, idErr)
		_, err = versionStmt.ExecContext(
			ctx, versionID, nodeID, fakeHash("a1"), 1, "application/octet-stream", stamp, operationID,
		)
		require.NoError(t, err)
		_, err = watchStmt.ExecContext(
			ctx, "archive", name, nodeID, fakeHash("a1"), 1,
		)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	rows, err := s.db.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT node_id, blob_hash, size FROM watch_sources
		WHERE watch_name = ? AND source_ref = ?`, "archive", "file-09999")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(t, plan, "USING PRIMARY KEY")
	assert.NotContains(t, plan, "SCAN watch_sources")
}

func TestIngestAndProvenanceFactsAreImmutable(t *testing.T) {
	s := newTestStore(t)
	ingestID, err := s.BeginIngest(t.Context(), "cli", "source")
	require.NoError(t, err)
	node, added, err := s.IngestFile(t.Context(), ingestID, s.RootID(),
		"a.txt", fakeHash("a1"), 1, "text/plain", "/source/a.txt", "")
	require.NoError(t, err)
	require.True(t, added)

	_, err = s.db.Exec(`UPDATE ingests SET source_desc='rewritten' WHERE id=?`, ingestID.ID())
	require.ErrorContains(t, err, "ingest records are immutable")
	_, err = s.db.Exec(`UPDATE provenance SET original_path='/rewritten' WHERE node_id=?`, node.ID)
	require.ErrorContains(t, err, "provenance records are immutable")

	var sourceDesc, originalPath string
	require.NoError(t, s.db.QueryRow(`SELECT source_desc FROM ingests WHERE id=?`, ingestID.ID()).Scan(&sourceDesc))
	require.NoError(t, s.db.QueryRow(`SELECT original_path FROM provenance WHERE node_id=?`, node.ID).Scan(&originalPath))
	assert.Equal(t, "source", sourceDesc)
	assert.Equal(t, "/source/a.txt", originalPath)
}

func TestIngestIdempotencyUsesActiveProvenanceLeaf(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ingestID, err := s.BeginIngest(ctx, "cli", "source")
	require.NoError(t, err)
	node, added, err := s.IngestFile(ctx, ingestID, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/obsolete/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)

	var priorIdentity string
	require.NoError(t, s.db.QueryRow(
		`SELECT identity FROM provenance WHERE node_id=?`, node.ID,
	).Scan(&priorIdentity))
	corrected := metadataProvenance{
		Type: metadataProvenanceType, NodeID: node.ID, IngestID: ingestID.ID(),
		OriginalPath: "/corrected/renamed.pdf", Supersedes: &priorIdentity,
	}
	corrected.Identity, err = provenanceIdentity(corrected)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO provenance(
		identity,node_id,ingest_id,original_path,original_mtime,supersedes
	) VALUES(?,?,?,?,?,?)`, corrected.Identity, corrected.NodeID, corrected.IngestID,
		corrected.OriginalPath, corrected.OriginalMTime, corrected.Supersedes)
	require.NoError(t, err)

	// The active origin identifies the existing node even though its current
	// virtual name belongs to a different suffix family.
	skipped, added, err := s.IngestFile(ctx, ingestID, s.RootID(),
		"renamed.pdf", fakeHash("a1"), 1, "application/pdf", "/corrected/renamed.pdf", "")
	require.NoError(t, err)
	require.False(t, added)
	assert.Equal(t, node.ID, skipped.ID)

	// The obsolete origin no longer identifies this node. An identical file
	// imported from that basename is a distinct source and must not be skipped.
	imported, added, err := s.IngestFile(ctx, ingestID, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/other/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.NotEqual(t, node.ID, imported.ID)
	assert.Equal(t, "report (2).pdf", imported.Name)
}

func TestBeginIngestAllocatesDistinctUUIDs(t *testing.T) {
	s := newTestStore(t)
	first, err := s.BeginIngest(t.Context(), "cli", "first")
	require.NoError(t, err)
	second, err := s.BeginIngest(t.Context(), "cli", "second")
	require.NoError(t, err)

	require.NoError(t, validateUUIDv4(first.ID()))
	require.NoError(t, validateUUIDv4(second.ID()))
	assert.NotEqual(t, first.ID(), second.ID())
}

func TestIngestRejectsNonUTF8MetadataBeforeCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ingestID, err := s.BeginIngest(ctx, "cli", "valid source")
	require.NoError(t, err)
	invalid := "source-" + string([]byte{0xff})
	_, _, err = s.IngestFile(ctx, ingestID, s.RootID(),
		"valid.txt", fakeHash("a1"), 1, "text/plain", invalid, "")
	require.ErrorContains(t, err, "provenance original_path: not valid UTF-8")
	_, err = s.NodeByPath(ctx, "/valid.txt")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.BeginIngest(ctx, "cli", invalid)
	require.ErrorContains(t, err, "ingest source_desc: not valid UTF-8")
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
}

func TestIngestFileDistinctSourceNamedLikeSuffix(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)

	// A real source file named like an auto-suffixed copy imports first...
	n1, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report (2).pdf", fakeHash("a1"), 1, "application/pdf", "/src/report (2).pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (2).pdf", n1.Name)

	// ...and an identical-content report.pdf is a distinct source file,
	// not a re-import: it must land under its own name.
	n2, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/src/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report.pdf", n2.Name)

	// Re-running both converges: each skips against its own prior import.
	_, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report (2).pdf", fakeHash("a1"), 1, "application/pdf", "/src/report (2).pdf", "")
	require.NoError(t, err)
	assert.False(t, added)
	_, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/src/report.pdf", "")
	require.NoError(t, err)
	assert.False(t, added)
}

func TestIngestFileDoesNotMatchUnknownOriginOutsideNameFamily(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	manual, err := s.CreateFile(ctx, s.RootID(), "other.bin", fakeHash("a1"), 1, "application/octet-stream")
	require.NoError(t, err)
	ingestID, err := s.BeginIngest(ctx, "cli", "source")
	require.NoError(t, err)

	imported, added, err := s.IngestFile(ctx, ingestID, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/source/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.NotEqual(t, manual.ID, imported.ID)
	assert.Equal(t, "report.pdf", imported.Name)
}
