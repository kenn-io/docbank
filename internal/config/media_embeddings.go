package config

import "fmt"

func validateSuppliedMediaEmbeddingProfile(c Config) error {
	name := c.SuppliedMediaEmbeddingProfile
	if name == "" {
		return nil
	}
	if _, ok := c.ProcessingProfiles[name]; !ok {
		return fmt.Errorf("supplied_media_embedding_profile %q is not a configured processing profile", name)
	}
	return nil
}
