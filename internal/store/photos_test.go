package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoAssetIDsForNodesKeepsDistinctMembership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first.jpg", browseHash("first"), 20, "image/jpeg")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second.jpg", browseHash("second"), 20, "image/jpeg")
	require.NoError(t, err)
	a, err := s.PhotoAssetForNode(t.Context(), first.ID)
	require.NoError(t, err)
	b, err := s.PhotoAssetForNode(t.Context(), second.ID)
	require.NoError(t, err)
	ids, err := s.PhotoAssetIDsForNodes(t.Context(), []int64{second.ID, first.ID, first.ID, 999999}, false)
	require.NoError(t, err)
	require.Equal(t, map[int64]string{first.ID: a.ID, second.ID: b.ID}, ids)

	duplicate, err := s.CreateFile(t.Context(), s.RootID(), "harbor.jpg", browseHash("first"), 20, "image/jpeg")
	require.NoError(t, err)
	duplicateAsset, err := s.PhotoAssetForNode(t.Context(), duplicate.ID)
	require.NoError(t, err)
	ids, err = s.PhotoAssetIDsForNodes(t.Context(), []int64{duplicate.ID}, true)
	require.NoError(t, err)
	require.Equal(t, map[int64]string{duplicate.ID: duplicateAsset.ID}, ids)

	ids, err = s.PhotoAssetIDsForNodes(t.Context(), []int64{duplicate.ID, first.ID}, true)
	require.NoError(t, err)
	require.Equal(t, map[int64]string{first.ID: a.ID}, ids)
}
