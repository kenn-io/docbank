package processing

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func newMixedImageTextProfileService(t *testing.T, f *photoDescriptionFixture, x photoSearchFixture) (*Service, string, string) {
	t.Helper()
	var base document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(x.fake.profile.CanonicalProfile, &base))
	original := base.Embeddings[1]
	image := x.binding
	image.Name = "image"
	portable := x.portable
	portable.Embeddings = []document.EmbeddingBindingV1{image, original}
	service, err := NewService(t.Context(), ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(), Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"mixed": {Profile: portable, EmbeddingProviders: map[string]document.EmbeddingProvider{
		image.Name:    &embeddingWorkerProvider{runtime: x.fake.runtime, binding: image.Name, descriptor: x.descriptor},
		original.Name: &embeddingWorkerProvider{runtime: x.fake.runtime, binding: original.Name, descriptor: x.fake.descriptor},
	}}}})
	require.NoError(t, err)
	return service, original.Name, image.Name
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

func TestDocumentSearchSelectsTextBindingInMixedProfile(t *testing.T) {
	t.Parallel()
	t.Run("image-only profile", func(t *testing.T) {
		x := newImageEmbeddingService(t, newPhotoDescriptionFixture(t))
		report, err := x.service.Search(t.Context(), SearchRequest{Profile: "images", Query: "unmatchednebula", Mode: "hybrid", Fence: SourceFence{VaultUID: x.f.catalog.VaultID(), ContentVersionIDs: []string{x.f.node.CurrentVersionID}}})
		require.NoError(t, err)
		require.Equal(t, "hybrid", string(report.RequestedMode))
		require.Equal(t, "lexical", string(report.ActualMode))
		require.Empty(t, report.Results)
		require.Empty(t, x.service.Profiles()[0].QueryEmbeddingBindings)
	})
	t.Run("mixed profile", func(t *testing.T) {
		f := newPhotoDescriptionFixture(t)
		x := newImageEmbeddingService(t, f)
		service, original, image := newMixedImageTextProfileService(t, f, x)
		require.Equal(t, []string{original}, service.Profiles()[0].QueryEmbeddingBindings)
		prepared, err := service.prepareSearch(SearchRequest{Query: "synthetic", Mode: "hybrid"}, service.profiles["mixed"])
		require.NoError(t, err)
		require.Equal(t, original, prepared.bindingID)
		err = service.ValidateSearch(t.Context(), SearchRequest{Profile: "mixed", Query: "synthetic", Mode: "hybrid", BindingID: image})
		require.ErrorIs(t, err, store.ErrNotFound)
	})
}
