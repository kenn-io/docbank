package processing

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type blockedFacePayload struct {
	store.RenditionBlobReader

	hash             string
	entered, release chan struct{}
	err              error
}

func (r *blockedFacePayload) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	if r.err != nil {
		return nil, 0, r.err
	}
	if hash == r.hash {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	return r.RenditionBlobReader.OpenStreamContext(ctx, hash)
}

func TestFacePickerCachedVectorsRemainAvailableDuringColdRead(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	other := x.f.addPhoto(t, "unrelated.jpg", 703)
	_, _, err := x.f.catalog.CreatePhotoFace(t.Context(), other, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	x.f.grant(t, x.service, config.LocalFacesProfile)
	_, err = x.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	snapshot, err := x.f.catalog.FaceMatchingInputs(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Sets, 2)
	warm := store.FaceMatchingSnapshot{}
	var coldHash string
	for _, item := range snapshot.Sets {
		warm.ReferencedSetIDs = append(warm.ReferencedSetIDs, item.Set.ID)
		if item.Set.ContentVersionID == x.f.node.CurrentVersionID {
			warm.Sets = append(warm.Sets, item)
		} else {
			coldHash = item.Set.VectorSet.PayloadBlobHash
		}
	}
	vectors, err := x.f.catalog.FaceMatchingVectors(t.Context(), x.f.blobs, warm, true)
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	synctest.Test(t, func(t *testing.T) {
		reader := &blockedFacePayload{RenditionBlobReader: x.f.blobs, hash: coldHash, entered: make(chan struct{}), release: make(chan struct{})}
		coldDone, warmDone := make(chan error, 1), make(chan error, 1)
		var cached []store.FaceVector
		go func() {
			_, err := x.f.catalog.FaceMatchingVectors(t.Context(), reader, snapshot, true)
			coldDone <- err
		}()
		<-reader.entered
		go func() {
			var err error
			cached, err = x.f.catalog.FaceMatchingVectors(t.Context(), &blockedFacePayload{err: errors.New("cached payload must not be opened")}, warm, true)
			warmDone <- err
		}()
		completed := false
		var warmErr error
		select {
		case warmErr = <-warmDone:
			completed = true
		case <-time.After(5 * time.Second):
			assert.Fail(t, "cached picker vectors must remain available while an unrelated payload read is blocked")
		}
		close(reader.release)
		require.NoError(t, <-coldDone)
		if !completed {
			warmErr = <-warmDone
		}
		require.NoError(t, warmErr)
		require.Len(t, cached, 2)
	})
}
