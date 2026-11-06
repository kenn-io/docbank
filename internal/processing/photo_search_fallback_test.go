package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchPhotosUnbuiltIndexFallback(t *testing.T) {
	t.Parallel()
	x := newImageEmbeddingService(t, newPhotoDescriptionFixture(t))
	t.Cleanup(x.service.Stop)
	_, err := x.f.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
		Principal: x.service.principal, Scope: x.service.scope,
		ProfileFingerprint:    x.service.profiles["images"].record.Fingerprint,
		DisclosureFingerprint: x.binding.DisclosureFingerprint,
		InputClasses:          []string{"query_text"}, RetainedArtifactClasses: []string{},
	})
	require.NoError(t, err)

	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			result, err := x.service.SearchPhotos(t.Context(), store.PhotoBrowseRequest{Query: query.Query{
				V: 1, Syntax: "simple", Mode: mode, Text: "harbor",
				Sort: query.Sort{Field: "relevance", Direction: "desc"},
			}})
			require.NoError(t, err)
			require.Equal(t, "index_unavailable", result.FallbackReason)
		})
	}
}
