package main

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"path/filepath"
	"testing"
)

func TestSelectedMediaDaemonSuppliedEmbeddingSetup(t *testing.T) {
	cfg, err := config.Default().WithLocalText()
	require.NoError(t, err)
	cfg.SuppliedMediaEmbeddingProfile = config.LocalTextProfile
	bundle, err := configureEmbeddingRuntimeBundle(cfg)
	require.NoError(t, err)
	profiles, err := executableProcessingProfiles(cfg, bundle)
	require.NoError(t, err)
	catalog, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	name, composed, err := configuredSuppliedMediaProfile(cfg, catalog, blobs, profiles)
	require.NoError(t, err)
	require.Equal(t, profiles[config.LocalTextProfile].Profile.Embeddings, composed.Profile.Embeddings)
	require.Equal(t, processing.SuppliedMediaProfileName, name)
	cfg.SuppliedMediaEmbeddingProfile = "missing"
	_, _, err = configuredSuppliedMediaProfile(cfg, catalog, blobs, profiles)
	require.ErrorContains(t, err, "no executable runtime")
}
