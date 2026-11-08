package processing

import (
	"bytes"
	"io"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
)

func TestUnavailableFacePayloadPreservesOpenReview(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		values := make([]float32, 512)
		if box.Y > .5 {
			values[1] = 1
		} else {
			values[0] = 1
		}
		return values
	}
	ctx := t.Context()
	healthy := x.f.addPhoto(t, "healthy.jpg", 700)
	for _, left := range []float64{.1, .6} {
		_, _, err := x.f.catalog.CreatePhotoFace(ctx, healthy, uuid.New().String(), document.FaceBox{X: left, Y: .6, Width: .2, Height: .3}, "")
		require.NoError(t, err)
	}
	x.f.grant(t, x.service, config.LocalFacesProfile)
	for range 2 {
		_, err := x.worker.ScanOnce(ctx)
		require.NoError(t, err)
	}
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	before, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, before, 2)
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	var hash string
	for _, item := range snapshot.Sets {
		if item.Set.ContentVersionID == x.f.node.CurrentVersionID {
			hash = item.Set.VectorSet.PayloadBlobHash
		}
	}
	source, err := x.f.blobs.OpenContext(ctx, hash)
	require.NoError(t, err)
	data, err := io.ReadAll(source)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, x.f.blobs.Remove(hash))
	service, err := NewService(ctx, ServiceConfig{Catalog: x.f.catalog, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	require.ErrorIs(t, service.rebuildFaceSuggestions(ctx), store.ErrVectorSetUnavailable)
	partial, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Equal(t, before, partial, "healthy faces and unreadable evidence must retain their open reviews")
	_, err = x.f.blobs.WriteDetailedContext(ctx, bytes.NewReader(data))
	require.NoError(t, err)
	require.NoError(t, service.rebuildFaceSuggestions(ctx))
	after, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, after, 2)
	require.Equal(t, before, after, "an unchanged review must survive payload recovery")
}

func TestUnavailableFaceBindingSharesVectorSpace(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	x.onRequest = func() { x.matchAll = x.calls == 0 }
	profile, _, err := config.LocalFaces()
	require.NoError(t, err)
	second := profile.Embeddings[0]
	second.Name = "second-faces"
	profile.Embeddings = append(profile.Embeddings, second)
	provider := x.service.profiles[config.LocalFacesProfile].embedders[config.LocalFacesProfile]
	ctx := t.Context()
	service, err := NewService(ctx, ServiceConfig{Catalog: x.f.catalog, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"dual-faces": {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider, second.Name: provider}}}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	x.worker.runtime = service.EmbeddingRuntimes()
	x.f.grant(t, service, "dual-faces")
	for range 2 {
		_, err := x.worker.ScanOnce(ctx)
		require.NoError(t, err)
	}
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot.Sets, 2)
	require.Equal(t, snapshot.Sets[0].Set.VectorSpace.ID, snapshot.Sets[1].Set.VectorSpace.ID)
	var hash string
	for _, item := range snapshot.Sets {
		if item.Set.VectorSet.RowCount == 2 {
			hash = item.Set.VectorSet.PayloadBlobHash
		}
	}
	require.NotEmpty(t, hash)
	require.NoError(t, service.rebuildFaceSuggestions(ctx))
	before, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.NoError(t, x.f.blobs.Remove(hash))
	restarted, err := NewService(ctx, ServiceConfig{Catalog: x.f.catalog, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx})
	require.NoError(t, err)
	t.Cleanup(restarted.Stop)
	require.ErrorIs(t, restarted.rebuildFaceSuggestions(ctx), store.ErrVectorSetUnavailable)
	after, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, after, 1, "a readable binding in the same space must not mask the unavailable binding")
	require.Equal(t, before[0].Revision, after[0].Revision)
}
