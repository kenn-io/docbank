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
	_, err = s.Trash(ctx, f.ID, f.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)

	// Nothing mutated: still live at the root under its original name.
	got, err := s.NodeByPath(ctx, "/f.txt")
	require.NoError(t, err)
	assert.Equal(t, f.Revision, got.Revision)

	trashed, err := s.Trash(ctx, f.ID, f.Revision)
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
	_, err = s.Move(ctx, f.ID, s.RootID(), "g.txt", -1)
	require.NoError(t, err)
}
