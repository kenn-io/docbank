package processing

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

// Interleave a real catalog removal at the first context check in the asset
// lookup, after retrieval has finished revalidating its candidates.
type photoLookupContext struct {
	context.Context

	once   sync.Once
	remove func()
}

func (ctx *photoLookupContext) Done() <-chan struct{} {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for frame, more := frames.Next(); frame.Function != ""; frame, more = frames.Next() {
		if frame.Function == "go.kenn.io/docbank/internal/store.(*Store).PhotoAssetIDsForNodes" {
			ctx.once.Do(ctx.remove)
			break
		}
		if !more {
			break
		}
	}
	return ctx.Context.Done()
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
	var removalErr error
	ctx := &photoLookupContext{Context: t.Context(), remove: func() {
		_, removalErr = f.catalog.TrashPhotoAsset(t.Context(), f.asset.ID, f.asset.Revision)
		if removalErr == nil {
			_, removalErr = f.catalog.TrashEmpty(t.Context(), 0, true)
		}
		removed = true
	}}
	ranked, err := x.service.SearchPhotos(ctx, store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "hybrid", Text: "sunset", Sort: query.Sort{Field: "relevance", Direction: "desc"}}})
	require.True(t, removed)
	require.NoError(t, removalErr)
	require.NoError(t, err)
	require.Empty(t, ranked.FallbackReason)
	require.Equal(t, "hybrid", ranked.ActualMode)
	require.Len(t, ranked.Members, 1)
	require.Equal(t, asset.ID, ranked.Members[0].AssetID)
}
