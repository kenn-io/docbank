package config

import (
	"bytes"
	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedMediaEmbeddingProfileConfiguration(t *testing.T) {
	for _, donor := range []string{"archive", "missing"} {
		t.Run(donor, func(t *testing.T) {
			cfg := validProcessingConfig()
			var encoded bytes.Buffer
			encoded.WriteString("supplied_media_embedding_profile = \"" + donor + "\"\n")
			require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{"server": cfg.Server, "rendition_profiles": cfg.RenditionProfiles, "embedding_profiles": cfg.EmbeddingProfiles, "retrieval_profiles": cfg.RetrievalProfiles, "processing_profiles": cfg.ProcessingProfiles}))
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), encoded.Bytes(), 0o600))
			_, err := Load(root)
			if donor == "archive" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "supplied_media_embedding_profile")
			}
		})
	}
}
