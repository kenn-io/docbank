package processing

import (
	"errors"
	"maps"
	"slices"

	"go.kenn.io/docbank/document"
)

// WithSuppliedMediaEmbeddings adds an explicitly selected donor's chunk
// embeddings to the built-in supplied transcript profile. Its rendition,
// source-input authority, retention and evidence policies remain unchanged.
// The composed profile has a new canonical identity and requires its own
// reviewed consent, just like any other processing profile change.
func WithSuppliedMediaEmbeddings(supplied, donor ProfileConfig) (ProfileConfig, error) {
	if supplied.Profile.Rendition == nil || supplied.Profile.Rendition.Name != SuppliedMediaProfileName {
		return ProfileConfig{}, errors.New("supplied media embeddings require the built-in transcript profile")
	}
	if len(donor.Profile.Embeddings) == 0 {
		return ProfileConfig{}, errors.New("supplied media embedding donor has no bindings")
	}
	for _, binding := range donor.Profile.Embeddings {
		if binding.InputKind != document.EmbeddingInputRenditionChunk {
			return ProfileConfig{}, errors.New("supplied media embedding donor requires rendition_chunk bindings")
		}
	}
	supplied.Profile.Embeddings = slices.Clone(donor.Profile.Embeddings)
	supplied.EmbeddingProviders = maps.Clone(donor.EmbeddingProviders)
	supplied.EmbeddingDisclosures = maps.Clone(donor.EmbeddingDisclosures)
	supplied.EmbeddingClassifiers = maps.Clone(donor.EmbeddingClassifiers)
	supplied.EmbeddingSimilarityFloors = maps.Clone(donor.EmbeddingSimilarityFloors)
	supplied.Tokenizers = maps.Clone(donor.Tokenizers)
	return supplied, nil
}
