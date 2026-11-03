package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

type selectedImageProvider struct{ *embeddingWorkerProvider }

func (p selectedImageProvider) VectorIdentity() (string, error) {
	return workerHash("selected-image-space"), nil
}

func TestPhotoEmbeddingPreservesSelectedVectorIdentity(t *testing.T) {
	t.Parallel()
	x := newPhotoSearchFixture(t, func(provider *embeddingWorkerProvider) document.EmbeddingProvider {
		return selectedImageProvider{provider}
	})
	result, err := x.service.SearchPhotos(t.Context(), store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "semantic", Text: "harbor", Sort: query.Sort{Field: "relevance", Direction: "desc"}}})
	require.NoError(t, err)
	require.Empty(t, result.FallbackReason)
	require.Equal(t, "semantic", result.ActualMode)
	require.Len(t, result.Members, 1)
	require.Equal(t, x.f.asset.ID, result.Members[0].AssetID)
}
