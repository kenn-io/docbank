package store

import (
	"bytes"
	"database/sql"
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

	// Third distinct content takes the next free ordinal.
	n3, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("c3"), 12, "application/pdf", "/more/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (3).pdf", n3.Name)
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
	require.NoError(t, validateUUIDv4(ing))
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
	assert.Equal(t, ing, storedIngestID)
	wantIdentity, err := provenanceIdentity(metadataProvenance{
		Type: metadataProvenanceType, NodeID: n.ID, IngestID: ing,
		OriginalPath: origPath, OriginalMTime: &origMtime,
	})
	require.NoError(t, err)
	assert.Equal(t, wantIdentity, identity)
}

func TestIngestAndProvenanceFactsAreImmutable(t *testing.T) {
	s := newTestStore(t)
	ingestID, err := s.BeginIngest(t.Context(), "cli", "source")
	require.NoError(t, err)
	node, added, err := s.IngestFile(t.Context(), ingestID, s.RootID(),
		"a.txt", fakeHash("a1"), 1, "text/plain", "/source/a.txt", "")
	require.NoError(t, err)
	require.True(t, added)

	_, err = s.db.Exec(`UPDATE ingests SET source_desc='rewritten' WHERE id=?`, ingestID)
	require.ErrorContains(t, err, "ingest records are immutable")
	_, err = s.db.Exec(`UPDATE provenance SET original_path='/rewritten' WHERE node_id=?`, node.ID)
	require.ErrorContains(t, err, "provenance records are immutable")

	var sourceDesc, originalPath string
	require.NoError(t, s.db.QueryRow(`SELECT source_desc FROM ingests WHERE id=?`, ingestID).Scan(&sourceDesc))
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
		Type: metadataProvenanceType, NodeID: node.ID, IngestID: ingestID,
		OriginalPath: "/corrected/renamed.pdf", Supersedes: &priorIdentity,
	}
	corrected.Identity, err = provenanceIdentity(corrected)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO provenance(
		identity,node_id,ingest_id,original_path,original_mtime,supersedes
	) VALUES(?,?,?,?,?,?)`, corrected.Identity, corrected.NodeID, corrected.IngestID,
		corrected.OriginalPath, corrected.OriginalMTime, corrected.Supersedes)
	require.NoError(t, err)

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

	require.NoError(t, validateUUIDv4(first))
	require.NoError(t, validateUUIDv4(second))
	assert.NotEqual(t, first, second)
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
