package processing

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.kenn.io/docbank/document"
)

// ComposeSuppliedMediaEmbeddings adds the configured donor when one is named.
func ComposeSuppliedMediaEmbeddings(supplied ProfileConfig, profiles map[string]ProfileConfig, name string) (ProfileConfig, error) {
	if name == "" {
		return supplied, nil
	}
	donor, ok := profiles[name]
	if !ok {
		return ProfileConfig{}, fmt.Errorf("supplied media embedding profile %q has no executable runtime", name)
	}
	return WithSuppliedMediaEmbeddings(supplied, donor)
}

// WithSuppliedMediaEmbeddings adds an explicitly selected donor's chunk
// embeddings to the built-in supplied transcript profile. Its rendition,
// source-input authority, retention and evidence policies remain unchanged.
// The composed profile has a new canonical identity and requires its own
// reviewed consent, just like any other processing profile change.
func WithSuppliedMediaEmbeddings(supplied, donor ProfileConfig) (ProfileConfig, error) {
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
