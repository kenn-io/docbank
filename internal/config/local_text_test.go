package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalTextKeepsRevisionEchoSourcesConfigured(t *testing.T) {
	cfg, err := Default().WithLocalText()
	require.NoError(t, err)
	profile := cfg.EmbeddingProfiles[LocalTextProfile]
	profile.Runtime.ProviderRevisionHeader = "X-Model-Revision"
	_, err = LocalTextEmbedding(profile, "local")
	require.EqualError(t, err, "model or input contract differs from local Nomic")
}
