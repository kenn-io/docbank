package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchMoveAppliesFinalTopologyAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	left, err := s.Mkdir(ctx, s.RootID(), "left")
	require.NoError(t, err)
	right, err := s.Mkdir(ctx, s.RootID(), "right")
	require.NoError(t, err)
	first, err := s.CreateFile(ctx, left.ID, "first.txt", fakeHash("first"), 5, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, right.ID, "second.txt", fakeHash("second"), 6, "text/plain")
	require.NoError(t, err)

	results, err := s.BatchMove(ctx, []BatchMoveRequest{
		{SourcePath: "/left/first.txt", DestinationPath: "/right/second.txt"},
		{NodeID: second.ID, IfRevision: second.Revision, DestinationPath: "/left/first.txt"},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, first.ID, results[0].Node.ID)
	assert.Equal(t, "/left/first.txt", results[0].FromPath)
	assert.Equal(t, "/right/second.txt", results[0].Path)
	assert.Equal(t, second.ID, results[1].Node.ID)
	assert.Equal(t, "/right/second.txt", results[1].FromPath)
	assert.Equal(t, "/left/first.txt", results[1].Path)

	atRight, err := s.NodeByPath(ctx, "/right/second.txt")
	require.NoError(t, err)
	assert.Equal(t, first.ID, atRight.ID)
	atLeft, err := s.NodeByPath(ctx, "/left/first.txt")
	require.NoError(t, err)
	assert.Equal(t, second.ID, atLeft.ID)
}

func TestBatchMoveSupportsNestedNetChanges(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.Mkdir(ctx, s.RootID(), "a")
	require.NoError(t, err)
	b, err := s.Mkdir(ctx, a.ID, "b")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "x")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "y")
	require.NoError(t, err)
	a, err = s.NodeByID(ctx, a.ID)
	require.NoError(t, err)
	b, err = s.NodeByID(ctx, b.ID)
	require.NoError(t, err)

	results, err := s.BatchMove(ctx, []BatchMoveRequest{
		{NodeID: a.ID, IfRevision: a.Revision, DestinationPath: "/x/a"},
		{NodeID: b.ID, IfRevision: b.Revision, DestinationPath: "/y/b"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/x/a", results[0].Path)
	assert.Equal(t, "/y/b", results[1].Path)
}

func TestBatchMoveResolvesDestinationParentsInFinalTree(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.Mkdir(ctx, s.RootID(), "a")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "x")
	require.NoError(t, err)
	item, err := s.CreateFile(ctx, s.RootID(), "item.txt", fakeHash("item"), 4, "text/plain")
	require.NoError(t, err)

	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{SourcePath: "/a", DestinationPath: "/x/a"},
		{SourcePath: "/item.txt", DestinationPath: "/a/item.txt"},
	})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.NodeByPath(ctx, "/a")
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/item.txt")
	require.NoError(t, err)

	results, err := s.BatchMove(ctx, []BatchMoveRequest{
		{SourcePath: "/a", DestinationPath: "/x/a"},
		{SourcePath: "/item.txt", DestinationPath: "/x/a/item.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/x/a", results[0].Path)
	assert.Equal(t, "/x/a/item.txt", results[1].Path)
	moved, err := s.NodeByPath(ctx, "/x/a/item.txt")
	require.NoError(t, err)
	assert.Equal(t, item.ID, moved.ID)
	assert.Equal(t, a.ID, *moved.ParentID)
}

func TestBatchMoveSwapsDirectoryCoordinates(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	left, err := s.Mkdir(ctx, s.RootID(), "left")
	require.NoError(t, err)
	right, err := s.Mkdir(ctx, s.RootID(), "right")
	require.NoError(t, err)
	leftChild, err := s.CreateFile(ctx, left.ID, "left.txt", fakeHash("left"), 4, "text/plain")
	require.NoError(t, err)
	rightChild, err := s.CreateFile(ctx, right.ID, "right.txt", fakeHash("right"), 5, "text/plain")
	require.NoError(t, err)

	results, err := s.BatchMove(ctx, []BatchMoveRequest{
		{SourcePath: "/left", DestinationPath: "/right"},
		{SourcePath: "/right", DestinationPath: "/left"},
	})
	require.NoError(t, err)
	assert.Equal(t, left.ID, results[0].Node.ID)
	assert.Equal(t, "/right", results[0].Path)
	assert.Equal(t, right.ID, results[1].Node.ID)
	assert.Equal(t, "/left", results[1].Path)
	leftResult, err := s.NodeByPath(ctx, "/right/left.txt")
	require.NoError(t, err)
	assert.Equal(t, leftChild.ID, leftResult.ID)
	rightResult, err := s.NodeByPath(ctx, "/left/right.txt")
	require.NoError(t, err)
	assert.Equal(t, rightChild.ID, rightResult.ID)
}

func TestBatchMoveCanOccupyVacatedDirectoryCoordinate(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	archive, err := s.Mkdir(ctx, s.RootID(), "archive")
	require.NoError(t, err)
	slot, err := s.Mkdir(ctx, s.RootID(), "slot")
	require.NoError(t, err)
	item, err := s.CreateFile(ctx, s.RootID(), "item.txt", fakeHash("item"), 4, "text/plain")
	require.NoError(t, err)

	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{SourcePath: "/slot", DestinationPath: "/archive/slot"},
		{SourcePath: "/item.txt", DestinationPath: "/slot"},
	})
	require.NoError(t, err)
	finalSlot, err := s.NodeByPath(ctx, "/slot")
	require.NoError(t, err)
	assert.Equal(t, item.ID, finalSlot.ID)
	archived, err := s.NodeByPath(ctx, "/archive/slot")
	require.NoError(t, err)
	assert.Equal(t, slot.ID, archived.ID)
	assert.Equal(t, archive.ID, *archived.ParentID)
}

func TestBatchMoveFailureRollsBackWholePlan(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	src, err := s.Mkdir(ctx, s.RootID(), "src")
	require.NoError(t, err)
	dst, err := s.Mkdir(ctx, s.RootID(), "dst")
	require.NoError(t, err)
	first, err := s.CreateFile(ctx, src.ID, "first.txt", fakeHash("first"), 5, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, src.ID, "second.txt", fakeHash("second"), 6, "text/plain")
	require.NoError(t, err)

	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{NodeID: first.ID, IfRevision: first.Revision, DestinationPath: "/dst/first.txt"},
		{NodeID: second.ID, IfRevision: second.Revision + 1, DestinationPath: "/dst/second.txt"},
	})
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.NodeByPath(ctx, "/src/first.txt")
	require.NoError(t, err)
	_, err = s.NodeByPath(ctx, "/src/second.txt")
	require.NoError(t, err)
	children, err := s.Children(ctx, dst.ID)
	require.NoError(t, err)
	assert.Empty(t, children)
}

func TestBatchMoveRejectsDuplicateSourceAndMissingFinalParent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.Mkdir(ctx, s.RootID(), "a")
	require.NoError(t, err)
	b, err := s.Mkdir(ctx, a.ID, "b")
	require.NoError(t, err)
	a, err = s.NodeByID(ctx, a.ID)
	require.NoError(t, err)

	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{NodeID: a.ID, IfRevision: a.Revision, DestinationPath: "/renamed"},
		{SourcePath: "/a", DestinationPath: "/other"},
	})
	require.ErrorIs(t, err, ErrInvalidBatchMove)

	_, err = s.BatchMove(ctx, []BatchMoveRequest{
		{NodeID: a.ID, IfRevision: a.Revision, DestinationPath: "/a/b/a"},
		{NodeID: b.ID, IfRevision: b.Revision, DestinationPath: "/a/b"},
	})
	require.ErrorIs(t, err, ErrNotFound)
}
