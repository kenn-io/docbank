package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveRename(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, docs.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	// Pure rename.
	renamed, err := s.Move(ctx, f.ID, docs.ID, "b.txt")
	require.NoError(t, err)
	assert.Equal(t, "b.txt", renamed.Name)
	assert.Equal(t, f.Revision+1, renamed.Revision)

	// Reparent to root.
	moved, err := s.Move(ctx, f.ID, s.RootID(), "b.txt")
	require.NoError(t, err)
	p, err := s.Path(ctx, moved.ID)
	require.NoError(t, err)
	assert.Equal(t, "/b.txt", p)
}

func TestMoveBumpsBothParents(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	src, err := s.Mkdir(ctx, s.RootID(), "src")
	require.NoError(t, err)
	dst, err := s.Mkdir(ctx, s.RootID(), "dst")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, src.ID, "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	srcBefore, err := s.NodeByID(ctx, src.ID)
	require.NoError(t, err)
	dstBefore, err := s.NodeByID(ctx, dst.ID)
	require.NoError(t, err)

	_, err = s.Move(ctx, f.ID, dst.ID, "a.txt")
	require.NoError(t, err)

	srcAfter, err := s.NodeByID(ctx, src.ID)
	require.NoError(t, err)
	dstAfter, err := s.NodeByID(ctx, dst.ID)
	require.NoError(t, err)
	assert.Equal(t, srcBefore.Revision+1, srcAfter.Revision)
	assert.Equal(t, dstBefore.Revision+1, dstAfter.Revision)
}

func TestMoveRejectsCycleCollisionRoot(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	a, err := s.Mkdir(ctx, s.RootID(), "a")
	require.NoError(t, err)
	b, err := s.Mkdir(ctx, a.ID, "b")
	require.NoError(t, err)

	// Cycle: a under its own child b; and a under itself.
	_, err = s.Move(ctx, a.ID, b.ID, "a")
	require.ErrorIs(t, err, ErrCycle)
	_, err = s.Move(ctx, a.ID, a.ID, "a")
	require.ErrorIs(t, err, ErrCycle)

	// Collision at destination.
	_, err = s.Mkdir(ctx, s.RootID(), "b")
	require.NoError(t, err)
	_, err = s.Move(ctx, b.ID, s.RootID(), "b")
	require.ErrorIs(t, err, ErrExists)

	// Root cannot move.
	_, err = s.Move(ctx, s.RootID(), a.ID, "root")
	require.ErrorIs(t, err, ErrIsRoot)

	// Destination must be a live dir.
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.Move(ctx, b.ID, f.ID, "b")
	assert.ErrorIs(t, err, ErrNotDir)
}
