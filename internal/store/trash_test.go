package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrashAndRestoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	require.NoError(t, s.Trash(ctx, docs.ID))

	// Subtree is gone from live views.
	_, err = s.NodeByPath(ctx, "/docs")
	require.ErrorIs(t, err, ErrNotFound)
	trashedChild, err := s.NodeByID(ctx, f.ID)
	require.NoError(t, err)
	assert.NotNil(t, trashedChild.TrashedAt)

	// Name is reusable while trashed.
	_, err = s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)

	// Restore re-suffixes because /docs is taken again.
	restored, err := s.Restore(ctx, docs.ID)
	require.NoError(t, err)
	assert.Equal(t, "docs (2)", restored.Name)
	assert.Nil(t, restored.TrashedAt)

	back, err := s.NodeByPath(ctx, "/docs (2)/a.txt")
	require.NoError(t, err)
	assert.Equal(t, f.ID, back.ID)
}

func TestNestedTrashRestoreKeepsEarlierTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	inner, err := s.Mkdir(ctx, docs.ID, "inner")
	require.NoError(t, err)

	// Trash inner first (separate operation), then the whole of docs.
	require.NoError(t, s.Trash(ctx, inner.ID))
	require.NoError(t, s.Trash(ctx, docs.ID))

	// Restoring docs must NOT resurrect inner.
	_, err = s.Restore(ctx, docs.ID)
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/docs/inner")
	require.ErrorIs(t, err, ErrNotFound)

	// inner is still restorable on its own.
	_, err = s.Restore(ctx, inner.ID)
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/docs/inner")
	require.NoError(t, err)
}

// deleteTrashRoot hard-deletes a node row directly — a test-only shortcut
// for "the original parent no longer exists".
func deleteTrashRoot(t *testing.T, s *Store, id int64) {
	t.Helper()
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	require.NoError(t, err)
}

func TestRestoreFallsBackToRootWhenParentGone(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	// f becomes its own trash root, then its original home disappears.
	require.NoError(t, s.Trash(ctx, f.ID))
	require.NoError(t, s.Trash(ctx, docs.ID))
	deleteTrashRoot(t, s, docs.ID)

	restored, err := s.Restore(ctx, f.ID)
	require.NoError(t, err)
	p, err := s.Path(ctx, restored.ID)
	require.NoError(t, err)
	assert.Equal(t, "/a.txt", p)
}

func TestTrashGuards(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	require.ErrorIs(t, s.Trash(ctx, s.RootID()), ErrIsRoot)

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	require.NoError(t, s.Trash(ctx, docs.ID))
	require.ErrorIs(t, s.Trash(ctx, docs.ID), ErrNotFound)

	// Restore of a non-trash-root child is refused.
	inner, err := s.Mkdir(ctx, s.RootID(), "x")
	require.NoError(t, err)
	leaf, err := s.CreateFile(ctx, inner.ID, "y.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.Trash(ctx, inner.ID))
	_, err = s.Restore(ctx, leaf.ID)
	assert.ErrorIs(t, err, ErrNotTrashed)
}

func TestEmptyTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	a, err := s.Mkdir(ctx, s.RootID(), "a")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, a.ID, "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.Trash(ctx, a.ID))

	roots, err := s.TrashedRoots(ctx)
	require.NoError(t, err)
	require.Len(t, roots, 1)

	// Nothing older than an hour.
	n, err := s.EmptyTrash(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Everything.
	n, err = s.EmptyTrash(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// Subtree rows are gone (cascade), blob row remains (GC's job).
	var nodeCount, blobCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount))
	assert.Equal(t, 1, nodeCount) // just the root
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount))
	assert.Equal(t, 1, blobCount)
}

func TestEmptyTrashWholeSecondTimestamp(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	// trashed_at is compared to the cutoff as a string, so a whole-second
	// timestamp must sort before a cutoff with fractional digits in the same
	// second. Under the old variable-width RFC3339Nano format it rendered
	// without fractional digits ("...:00Z" > "...:00.5Z") and survived.
	d, err := s.Mkdir(ctx, s.RootID(), "old")
	require.NoError(t, err)
	require.NoError(t, s.Trash(ctx, d.ID))
	stamp := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(timestampLayout)
	_, err = s.db.Exec(`UPDATE nodes SET trashed_at = ? WHERE id = ?`, stamp, d.ID)
	require.NoError(t, err)

	n, err := s.EmptyTrash(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestTrashPath(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	n, err := s.TrashPath(ctx, "/docs")
	require.NoError(t, err)
	assert.Equal(t, docs.ID, n.ID)

	// Gone from the live tree; restorable as a trash root.
	_, err = s.NodeByPath(ctx, "/docs")
	require.ErrorIs(t, err, ErrNotFound)
	roots, err := s.TrashedRoots(ctx)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, docs.ID, roots[0].ID)

	// The root and no-longer-resolving paths are refused.
	_, err = s.TrashPath(ctx, "/")
	require.ErrorIs(t, err, ErrIsRoot)
	_, err = s.TrashPath(ctx, "/docs")
	assert.ErrorIs(t, err, ErrNotFound)
}
