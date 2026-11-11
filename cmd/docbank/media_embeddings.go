package main

import (
	"fmt"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func configuredSuppliedMediaProfile(cfg config.Config, catalog *store.Store, blobs *blob.Store,
	profiles map[string]processing.ProfileConfig,
) (string, processing.ProfileConfig, error) {
	name, profile, err := processing.NewSuppliedMediaProfile(catalog, blobs, "daemon:operator")
	if err != nil || cfg.SuppliedMediaEmbeddingProfile == "" {
		return name, profile, err
	}
	donor, ok := profiles[cfg.SuppliedMediaEmbeddingProfile]
	if !ok {
		return "", processing.ProfileConfig{}, fmt.Errorf("supplied media embedding profile %q has no executable runtime", cfg.SuppliedMediaEmbeddingProfile)
	}
	profile, err = processing.WithSuppliedMediaEmbeddings(profile, donor)
	return name, profile, err
}
