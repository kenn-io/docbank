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

	// A version-only blob: referenced solely from node_versions.
	_, err = s.db.Exec(`INSERT INTO blobs (hash, size, created_at) VALUES (?, 9, ?)`,
		fakeHash("d4"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	_, err = s.db.Exec(
		`INSERT INTO node_versions (node_id, blob_hash, size, replaced_at) VALUES (?, ?, 9, ?)`,
		live.ID, fakeHash("d4"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)

	// Nothing unreachable yet.
	un, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, un)

	// Trashed-but-not-emptied stays reachable.
	require.NoError(t, s.Trash(ctx, trashed.ID))
	un, err = s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, un)

	// Hard-deleting a node makes its blob unreachable.
	require.NoError(t, s.Trash(ctx, gone.ID))
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
		`INSERT INTO extracted_text (blob_hash, extractor, extractor_version, status, extracted_at)
		 VALUES (?, 'pdf', 1, 'ok', ?)`, fakeHash("a1"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)

	require.NoError(t, s.DeleteBlobRows(ctx, []string{fakeHash("a1")}))

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM extracted_text`).Scan(&n))
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
