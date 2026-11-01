package processing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchPhotosDropsHitRemovedBeforeAssetLookup(t *testing.T) {
	t.Parallel()
	x := newPhotoSearchFixture(t)
	f := x.f
	other, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "sunset-other.jpg", f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
	require.NoError(t, err)
	asset, err := f.catalog.PhotoAssetForNode(t.Context(), other.ID)
	require.NoError(t, err)
	removed := false
	lookup := func(ctx context.Context, nodeIDs []int64, collapseDuplicates bool) (map[int64]string, error) {
		require.Contains(t, nodeIDs, f.node.ID)
		if _, err := f.catalog.TrashPhotoAsset(ctx, f.asset.ID, f.asset.Revision); err != nil {
			return nil, err
		}
		if _, err := f.catalog.TrashEmpty(ctx, 0, true); err != nil {
			return nil, err
		}
		removed = true
		return f.catalog.PhotoAssetIDsForNodes(ctx, nodeIDs, collapseDuplicates)
	}
	ranked, err := x.service.searchPhotosWithAssetLookup(t.Context(), store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "hybrid", Text: "sunset", Sort: query.Sort{Field: "relevance", Direction: "desc"}}}, lookup)
	require.True(t, removed)
	require.NoError(t, err)
	require.Empty(t, ranked.FallbackReason)
	require.Equal(t, "hybrid", ranked.ActualMode)
	require.Len(t, ranked.Members, 1)
	require.Equal(t, asset.ID, ranked.Members[0].AssetID)
}
