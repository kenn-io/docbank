package processing

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchPhotosHybridCollapsesOnlyAcceptedDuplicates(t *testing.T) {
	t.Parallel()
	x := newPhotoSearchFixture(t)
	// The older duplicate wins the common eligibility order, but only the
	// newer duplicate's filename matches. Neither has a qualifying vector.
	duplicate, err := x.f.catalog.CreateFile(t.Context(), x.f.catalog.RootID(), "harbor.jpg", x.f.written.Hash, x.f.written.Size, "image/jpeg", processingBlobPhysical(t, x.f.written))
	require.NoError(t, err)
	asset, err := x.f.catalog.PhotoAssetForNode(t.Context(), duplicate.ID)
	require.NoError(t, err)
	x.fake.runtime.mutate[x.binding.Name] = func(result document.EmbeddingResult) document.EmbeddingResult {
		result.Vectors[0].Values = []float32{2, -1}
		return result
	}
	request := store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "hybrid", Text: "harbor", Filters: query.Filters{CollapseDuplicates: true}, Sort: query.Sort{Field: "relevance", Direction: "desc"}}}
	ranked, err := x.service.SearchPhotos(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "hybrid", ranked.ActualMode)
	require.Len(t, ranked.Members, 1)
	require.Equal(t, asset.ID, ranked.Members[0].AssetID)

	request.Query.Text = "jpg"
	ranked, err = x.service.SearchPhotos(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, ranked.Members, 1)
	require.Equal(t, x.f.asset.ID, ranked.Members[0].AssetID)
}

func TestPhotoSemanticScopeKeepsBarePickFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, syntax, text string
		meaning, scope     string
		supported          bool
	}{
		{"advanced bare pick", "advanced", "sunset pick", "sunset", "pick", true},
		{"advanced quoted pick", "advanced", `sunset "pick"`, "sunset pick", "", true},
		{"simple pick", "simple", "sunset pick", "sunset pick", "", true},
		{"advanced pick alone", "advanced", "pick", "", "pick", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meaning, scope, supported := photoSemanticScope(query.Query{V: 1, Mode: "semantic", Syntax: tc.syntax, Text: tc.text})
			require.Equal(t, tc.supported, supported)
			if !supported {
				return
			}
			require.Equal(t, tc.meaning, meaning)
			require.Equal(t, tc.scope, scope.Text)
		})
	}
}

func TestDocumentsHybridSearchExcludesImageBindings(t *testing.T) {
	t.Parallel()
	x := newPhotoSearchFixture(t)
	report, err := x.service.Search(t.Context(), SearchRequest{Profile: "images", Query: "unmatchednebula", Mode: "hybrid", Fence: SourceFence{VaultUID: x.f.catalog.VaultID(), ContentVersionIDs: []string{x.f.node.CurrentVersionID}}})
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.Empty(t, x.service.Profiles()[0].QueryEmbeddingBindings)
}

func TestDocumentSearchSelectsTextBindingInMixedProfile(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	x := newImageEmbeddingService(t, f)
	var base document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(x.fake.profile.CanonicalProfile, &base))
	original := base.Embeddings[1]
	image := x.binding
	image.Name = "image"
	portable := x.portable
	portable.Embeddings = []document.EmbeddingBindingV1{image, original}
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(), Profiles: map[string]ProfileConfig{"mixed": {Profile: portable, EmbeddingProviders: map[string]document.EmbeddingProvider{
		image.Name:    &embeddingWorkerProvider{runtime: x.fake.runtime, binding: image.Name, descriptor: x.descriptor},
		original.Name: &embeddingWorkerProvider{runtime: x.fake.runtime, binding: original.Name, descriptor: x.fake.descriptor},
	}}}})
	require.NoError(t, err)
	require.Equal(t, []string{original.Name}, service.Profiles()[0].QueryEmbeddingBindings)
	prepared, err := service.prepareSearch(SearchRequest{Query: "synthetic", Mode: "hybrid"}, service.profiles["mixed"])
	require.NoError(t, err)
	require.Equal(t, original.Name, prepared.bindingID)
	err = service.ValidateSearch(t.Context(), SearchRequest{Profile: "mixed", Query: "synthetic", Mode: "hybrid", BindingID: image.Name})
	require.ErrorIs(t, err, store.ErrNotFound)
}
