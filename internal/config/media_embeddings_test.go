package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

func TestSelectedMediaEmbeddingProfileConfiguration(t *testing.T) {
	cfg := validProcessingConfig()
	var encoded bytes.Buffer
	encoded.WriteString("supplied_media_embedding_profile = \"archive\"\n")
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{"rendition_profiles": cfg.RenditionProfiles, "embedding_profiles": cfg.EmbeddingProfiles, "retrieval_profiles": cfg.RetrievalProfiles, "processing_profiles": cfg.ProcessingProfiles}))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), encoded.Bytes(), 0o600))
	loaded, err := Load(root)
	require.NoError(t, err)
	require.NoError(t, loaded.Validate())
	require.Equal(t, "archive", loaded.SuppliedMediaEmbeddingProfile)
}
