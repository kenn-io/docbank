package store

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeginWalkRejectsOversizedPage(t *testing.T) {
	s := newTestStore(t)

	walker, err := s.BeginWalk(t.Context(), "/", 5001, false)
	if walker != nil {
		t.Cleanup(func() { require.NoError(t, walker.Close()) })
	}
	require.Nil(t, walker)
	require.ErrorContains(t, err, "walk page size must be between 1 and 5000")
}

func TestWalkOrdersDuplicatePathsByNodeIDAndOptionallyIncludesTrash(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(
		t.Context(), s.RootID(), "same.txt", fakeHash("81"), 5, "text/plain",
	)
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), first.ID, first.Revision)
	require.NoError(t, err)
	live, err := s.CreateFile(
		t.Context(), s.RootID(), "same.txt", fakeHash("82"), 4, "text/plain",
	)
	require.NoError(t, err)
	root, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)

	withTrash, err := s.BeginWalk(t.Context(), "/", 1, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, withTrash.Close()) })
	assert.Equal(t, []WalkEntry{
		{Path: "/", Node: root},
		{Path: "/same.txt", Node: trashed},
		{Path: "/same.txt", Node: live},
	}, collectStoreWalk(t, withTrash))

	liveOnly, err := s.BeginWalk(t.Context(), "/", 1, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, liveOnly.Close()) })
	assert.Equal(t, []WalkEntry{
		{Path: "/", Node: root},
		{Path: "/same.txt", Node: live},
	}, collectStoreWalk(t, liveOnly))
}

func collectStoreWalk(t *testing.T, walker *Walker) []WalkEntry {
	t.Helper()
	var entries []WalkEntry
	for {
		page, err := walker.Next(t.Context())
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			return entries
		}
		entries = append(entries, page...)
	}
}
