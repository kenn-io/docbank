package processing

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchPhotosHybridCapsDistinctPhotosAfterFusion(t *testing.T) {
	t.Parallel()
	x := newPhotoSearchFixture(t)
	f := x.f
	// The older copy has a vector but no matching filename. The lexical lane
	// selects its newer duplicate before the remaining 999 distinct photos.
	_, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "sunset-0000.jpg", f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
	require.NoError(t, err)
	var lastAssetID string
	jpeg := mediatest.JPEG(16, 16, color.White)
	for i := 1; i <= 999; i++ {
		written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(append(jpeg, []byte(fmt.Sprintf("synthetic distinct photo %d", i))...)))
		require.NoError(t, err)
		node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), fmt.Sprintf("sunset-%04d.jpg", i), written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
		require.NoError(t, err)
		asset, err := f.catalog.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		lastAssetID = asset.ID
	}
	ranked, err := x.service.SearchPhotos(t.Context(), store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "hybrid", Text: "sunset", Filters: query.Filters{CollapseDuplicates: true}, Sort: query.Sort{Field: "relevance", Direction: "desc"}}})
	require.NoError(t, err)
	require.Empty(t, ranked.FallbackReason)
	require.Equal(t, "hybrid", ranked.ActualMode)
	assets := make(map[string]bool, len(ranked.Members))
	for _, member := range ranked.Members {
		assets[member.AssetID] = true
	}
	require.Len(t, assets, 1000, "two copies of one image must leave room for the next distinct photo")
	require.True(t, assets[lastAssetID], "the final lexical candidate must survive asset collapse")
	require.Len(t, ranked.Members, 1000)
}

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
