package config

import "fmt"

func validateSuppliedMediaEmbeddingProfile(c Config) error {
	name := c.SuppliedMediaEmbeddingProfile
	if name == "" {
		return nil
	}
	profile, ok := c.ProcessingProfiles[name]
	if !ok {
		return fmt.Errorf("supplied_media_embedding_profile %q is not a configured processing profile", name)
	}
	if len(profile.Embeddings) == 0 {
		return fmt.Errorf("supplied_media_embedding_profile %q has no embedding bindings", name)
	}
	for _, binding := range profile.Embeddings {
		if c.EmbeddingProfiles[binding].InputKind != "rendition_chunk" {
			return fmt.Errorf("supplied_media_embedding_profile %q requires rendition_chunk bindings", name)
		}
	}
	return nil
}
