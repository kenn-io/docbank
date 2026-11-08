package processing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

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
	service, err := NewService(ctx, ServiceConfig{Catalog: x.f.catalog, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{config.LocalFacesProfile: {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider, second.Name: provider}}}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	x.worker.runtime = service.EmbeddingRuntimes()
	setup := FaceSetup{Service: service, Startup: func(context.Context) error { return nil }}
	plan, err := setup.Plan(ctx)
	require.NoError(t, err)
	_, err = setup.Apply(ctx, plan.Fingerprint)
	require.NoError(t, err)
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
