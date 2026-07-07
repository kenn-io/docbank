package store

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMkdirAndLookup(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	docs, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	assert.Equal(t, "docs", docs.Name)
	assert.True(t, docs.IsDir())
	assert.Equal(t, int64(1), docs.Revision)

	byID, err := s.NodeByID(ctx, docs.ID)
	require.NoError(t, err)
	assert.Equal(t, docs.ID, byID.ID)

	byPath, err := s.NodeByPath(ctx, "/docs")
	require.NoError(t, err)
	assert.Equal(t, docs.ID, byPath.ID)

	root, err := s.NodeByPath(ctx, "/")
	require.NoError(t, err)
	assert.Equal(t, s.RootID(), root.ID)

	_, err = s.NodeByPath(ctx, "/nope")
	require.ErrorIs(t, err, ErrNotFound)

	p, err := s.Path(ctx, docs.ID)
	require.NoError(t, err)
	assert.Equal(t, "/docs", p)
}

func TestPathOnMissingNodeReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.Path(ctx, 99999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMkdirRejectsCollisionAndBadNames(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "docs")
	require.ErrorIs(t, err, ErrExists)
	_, err = s.Mkdir(ctx, s.RootID(), "a/b")
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestMkdirBumpsParentRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	before, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	after, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	assert.Equal(t, before.Revision+1, after.Revision)
}

func TestMkdirAllCreatesIntermediates(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	leaf, err := s.MkdirAll(ctx, "/a/b/c")
	require.NoError(t, err)
	p, err := s.Path(ctx, leaf.ID)
	require.NoError(t, err)
	assert.Equal(t, "/a/b/c", p)

	// Idempotent.
	again, err := s.MkdirAll(ctx, "/a/b/c")
	require.NoError(t, err)
	assert.Equal(t, leaf.ID, again.ID)
}

func TestMkdirAllConcurrent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	const n = 8
	var wg sync.WaitGroup
	ids := make([]int64, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leaf, err := s.MkdirAll(ctx, "/a/b/c")
			ids[i] = leaf.ID
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i])
	}

	kids, err := s.Children(ctx, s.RootID())
	require.NoError(t, err)
	require.Len(t, kids, 1)
	assert.Equal(t, "a", kids[0].Name)
}

func TestChildrenSortedDirsFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.Mkdir(ctx, s.RootID(), "zdir")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "adir")
	require.NoError(t, err)

	kids, err := s.Children(ctx, s.RootID())
	require.NoError(t, err)
	require.Len(t, kids, 2)
	assert.Equal(t, "adir", kids[0].Name)
	assert.Equal(t, "zdir", kids[1].Name)

	_, err = s.Children(ctx, kids[0].ID)
	require.NoError(t, err)
}
