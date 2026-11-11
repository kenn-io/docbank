package main

import (
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func configuredSuppliedMediaProfile(cfg config.Config, catalog *store.Store, blobs *blob.Store,
	profiles map[string]processing.ProfileConfig,
) (string, processing.ProfileConfig, error) {
	name, profile, err := processing.NewSuppliedMediaProfile(catalog, blobs, "daemon:operator")
	if err != nil {
		return name, profile, err
	}
	profile, err = processing.ComposeSuppliedMediaEmbeddings(profile, profiles, cfg.SuppliedMediaEmbeddingProfile)
	return name, profile, err
}
