package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/openaicompat"
)

func TestLocalTextRejectsLosingRevisionEchoAndReservedProfile(t *testing.T) {
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
	require.Error(t, err, "the managed server cannot preserve the source revision attestation")
	require.Equal(t, "X-Model-Revision", profile.Runtime.ProviderRevisionHeader)
	cfg.ProcessingProfiles[LocalTextProfile] = cfg.ProcessingProfiles["archive"]
	_, err = cfg.WithLocalText()
	require.ErrorContains(t, err, "reserved for local text")
}
