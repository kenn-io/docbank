package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaleRevisionRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	d, err := s.Mkdir(ctx, s.RootID(), "d")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	_, err = s.Move(ctx, f.ID, d.ID, "f.txt", f.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, _, err = s.Trash(ctx, f.ID, f.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)

	// Nothing mutated: still live at the root under its original name.
	got, err := s.NodeByPath(ctx, "/f.txt")
	require.NoError(t, err)
	assert.Equal(t, f.Revision, got.Revision)

	trashed, _, err := s.Trash(ctx, f.ID, f.Revision)
	require.NoError(t, err)
	assert.NotNil(t, trashed.TrashedAt)
	_, err = s.Restore(ctx, f.ID, trashed.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.Restore(ctx, f.ID, trashed.Revision)
	require.NoError(t, err)
}

func TestNegativeRevisionSkipsCheck(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.Move(ctx, f.ID, s.RootID(), "g.txt", UnconditionalRev)
	require.NoError(t, err)
}

// Only UnconditionalRev (-1) skips the precondition. Any other negative can
// never equal a real revision, so it must fail stale — an accidentally
// propagated bad value must not silently mutate.
func TestBelowSentinelRevisionFailsStale(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	_, err = s.Move(ctx, f.ID, s.RootID(), "g.txt", -2)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, _, err = s.Trash(ctx, f.ID, -2)
	require.ErrorIs(t, err, ErrStaleRevision)

	// Untouched: still live under its original name.
	_, err = s.NodeByPath(ctx, "/f.txt")
	require.NoError(t, err)

	trashed, _, err := s.Trash(ctx, f.ID, f.Revision)
	require.NoError(t, err)
	_, err = s.Restore(ctx, f.ID, -2)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.Restore(ctx, f.ID, trashed.Revision)
	require.NoError(t, err)
}
