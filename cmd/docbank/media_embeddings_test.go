package main

import (
	"slices"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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
	_, base, err := processing.NewSuppliedMediaProfile(catalog, blobs, "daemon:operator")
	require.NoError(t, err)
	name, composed, err := configuredSuppliedMediaProfile(cfg, catalog, blobs, profiles)
	require.NoError(t, err)
	require.Equal(t, base.Profile.Rendition, composed.Profile.Rendition)
	require.Equal(t, base.Profile.EvidenceLexical, composed.Profile.EvidenceLexical)
	require.Equal(t, base.Profile.RetentionDisclosure, composed.Profile.RetentionDisclosure)
	require.Equal(t, base.RenditionProvider.Descriptor(), composed.RenditionProvider.Descriptor())
	require.NotEmpty(t, composed.Profile.Embeddings)
	require.Equal(t, processing.SuppliedMediaProfileName, name)
	donor := profiles[config.LocalTextProfile]
	withoutBindings := donor
	withoutBindings.Profile.Embeddings = nil
	profiles["empty"] = withoutBindings
	wrongKind := donor
	wrongKind.Profile.Embeddings = slices.Clone(donor.Profile.Embeddings)
	wrongKind.Profile.Embeddings[0].InputKind = document.EmbeddingInputOriginalFile
	profiles["original"] = wrongKind
	for _, tc := range []struct{ name, message string }{{"empty", "no bindings"}, {"original", "requires rendition_chunk"}} {
		cfg.SuppliedMediaEmbeddingProfile = tc.name
		_, _, err := configuredSuppliedMediaProfile(cfg, catalog, blobs, profiles)
		require.ErrorContains(t, err, tc.message)
	}
	cfg.SuppliedMediaEmbeddingProfile = "missing"
	_, _, err = configuredSuppliedMediaProfile(cfg, catalog, blobs, profiles)
	require.ErrorContains(t, err, "no executable runtime")
}
