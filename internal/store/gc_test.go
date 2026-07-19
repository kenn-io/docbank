package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnreachableBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	live, err := s.CreateFile(ctx, s.RootID(), "live.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, s.RootID(), "trashed.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	gone, err := s.CreateFile(ctx, s.RootID(), "gone.txt", fakeHash("c3"), 1, "text/plain")
	require.NoError(t, err)

	// Replacing the current pointer leaves the original version as a root.
	_, err = s.db.Exec(`INSERT INTO blobs (hash, size, created_at) VALUES (?, 9, ?)`,
		fakeHash("d4"), "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = s.db.Exec(
		`INSERT INTO content_versions (
			version_id, node_id, blob_hash, size, recorded_at, node_revision,
			introduced_operation_id, transition_kind
		) VALUES ('44444444-4444-4444-8444-444444444444', ?, ?, 9, ?, 2,
			'dddddddd-dddd-4ddd-8ddd-dddddddddddd', 'content_replace')`,
		live.ID, fakeHash("d4"), "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE nodes SET current_version_id =
		'44444444-4444-4444-8444-444444444444', revision = 2 WHERE id = ?`, live.ID)
	require.NoError(t, err)

	// Nothing unreachable yet.
	un, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, un)

	// Trashed-but-not-emptied stays reachable.
	_, _, err = s.Trash(ctx, trashed.ID, -1)
	require.NoError(t, err)
	un, err = s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, un)

	// Hard-deleting a node makes its blob unreachable.
	_, _, err = s.Trash(ctx, gone.ID, -1)
	require.NoError(t, err)
	// Only 'gone' and 'trashed' are in the trash; empty just 'gone' by
	// deleting it directly (EmptyTrash cutoffs are time-based).
	_, err = s.db.Exec(`DELETE FROM nodes WHERE id = ?`, gone.ID)
	require.NoError(t, err)

	un, err = s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, un, 1)
	assert.Equal(t, fakeHash("c3"), un[0].Hash)
}

func TestDeleteBlobRows(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.db.Exec(`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
		fakeHash("a1"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	_, err = s.db.Exec(
		`INSERT INTO extracted_text (blob_hash, extractor, extractor_version, status, attempts, text, extracted_at)
		 VALUES (?, 'plain-text', 1, 'ok', 1, 'searchable', ?)`,
		fakeHash("a1"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO content_fts(rowid,blob_hash,extractor,text)
		SELECT rowid,blob_hash,extractor,text FROM extracted_text WHERE blob_hash=?`, fakeHash("a1"))
	require.NoError(t, err)

	require.NoError(t, s.DeleteBlobRows(ctx, []string{fakeHash("a1")}))

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM extracted_text`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestAllBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.CreateFile(ctx, s.RootID(), "b.txt", fakeHash("b2"), 2, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	blobs, err := s.AllBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, blobs, 2)
	assert.Equal(t, fakeHash("a1"), blobs[0].Hash) // hash-ordered
	assert.Equal(t, int64(1), blobs[0].Size)
}
