package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
)

func TestLocalTextConfigurationRejections(t *testing.T) {
	cfg := validProcessingConfig()
	cfg.CredentialBindings = map[string]CredentialBindingConfig{"embedding-primary": {EnvironmentVariable: "DOCBANK_TEST_REVISION_KEY"}}
	profile := cfg.EmbeddingProfiles["semantic"]
	profile.Model, profile.Dimensions = "nomic-embed-text", 768
	profile.DescriptorID, profile.TrustBoundary = openaicompat.ProviderID, string(document.EmbeddingTrustOperatorNetwork)
	profile.DocumentFormatter, profile.QueryFormatter = openaicompat.DocumentFormatterV1, openaicompat.QueryFormatterV1
	profile.ModelInput, profile.CompatibilityID = EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)}, "nomic/search/v1"
	profile.Runtime = &EmbeddingRuntimeConfig{AdapterContract: "docbank-openai-compatible-embeddings/v1",
		Endpoint: "http://127.0.0.1:12345", ModelRevision: "source-revision", ProviderRevisionHeader: "X-Model-Revision",
		RequestTimeout: Duration(time.Second), MaxRequestBytes: 1 << 20, AllowedCIDRs: []string{"127.0.0.0/8"},
		ProxyMode: "disabled", ConnectTimeout: Duration(time.Second), KeepAlive: Duration(time.Second), TLSHandshakeTimeout: Duration(time.Second)}
	cfg.EmbeddingProfiles["semantic"] = profile
	require.NoError(t, cfg.Validate())
	_, err := LocalTextEmbedding(profile, "local")
	require.ErrorContains(t, err, "openaicompat: exactly one deployment epoch or provider revision header is required")
	require.False(t, profile.Runtime.Managed)
	require.Empty(t, profile.Runtime.ServerMode)
	cfg.ProcessingProfiles[LocalTextProfile] = cfg.ProcessingProfiles["archive"]
	_, err = cfg.WithLocalText()
	require.ErrorContains(t, err, "reserved for local text")
}

func TestLocalTextPreservesFormattedInputCapacity(t *testing.T) {
	cfg, err := Default().WithLocalText()
	require.NoError(t, err)
	profile := cfg.EmbeddingProfiles[LocalTextProfile]
	profile.ModelInput = EmbeddingModelInputConfig{Profile: "custom/v1", CompatibilityID: profile.CompatibilityID,
		Document: EmbeddingModelInputEncoderConfig{Mode: "text", Template: "{{content}}"},
		Query:    EmbeddingModelInputEncoderConfig{Mode: "text", Template: "{{content}}"}}
	profile.MaxInputTokens = 2046
	moved, err := LocalTextEmbedding(profile, "local")
	require.NoError(t, err)
	require.Equal(t, profile.ModelInput, moved.ModelInput)
	profile.MaxInputTokens = 2047
	_, err = LocalTextEmbedding(profile, "local")
	require.ErrorContains(t, err, "model or input contract differs from local Nomic")
}

func TestLocalTextTransportDefaults(t *testing.T) {
	cfg, err := Default().WithLocalText()
	require.NoError(t, err)
	local := cfg.EmbeddingProfiles[LocalTextProfile]
	require.Equal(t, 8, local.MaxBatchItems)
	require.Equal(t, 5*time.Minute, local.Runtime.RequestTimeout.Std())

	local.MaxBatchItems = 23
	local.Runtime.RequestTimeout = Duration(42 * time.Second)
	local.Runtime.Endpoint = "http://127.0.0.1:12345"
	local.Runtime.Managed = false
	cfg.EmbeddingProfiles["remote"] = local
	remote, err := cfg.EmbeddingProfile("remote")
	require.NoError(t, err)
	require.Equal(t, 23, remote.MaxBatchItems)
	require.Equal(t, 42*time.Second, remote.Runtime.RequestTimeout.Std())
	moved, err := LocalTextEmbedding(remote, "local")
	require.NoError(t, err)
	require.Equal(t, 23, moved.MaxBatchItems)
	require.Equal(t, 42*time.Second, moved.Runtime.RequestTimeout.Std())
}
