package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/internal/config"
)

func TestConfigureVoyageRetainedTextRuntime(t *testing.T) {
	cfg, profile := voyageTextRuntimeConfig(t)
	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	descriptor := bundle.providers["voyage-text"].Descriptor()
	assert.True(t, descriptor.SupportsTextQuery)
	assert.Equal(t, profile.DescriptorFingerprint, descriptor.Fingerprint)

	profiles, err := executableProcessingProfiles(cfg, bundle)
	require.NoError(t, err)
	require.Contains(t, profiles, "private-text", "a configured Voyage text binding must produce an executable processing profile")
	configured := profiles["private-text"]
	assert.Same(t, bundle.providers["voyage-text"], configured.EmbeddingProviders["voyage-text"])
	require.NotNil(t, configured.Tokenizers["voyage-text"])
	disclosure := configured.EmbeddingDisclosures["voyage-text"]
	assert.Equal(t, voyageEmbeddingAdapter, disclosure.ImmediateProcessor)
	assert.Equal(t, voyage.EmbeddingProviderID, disclosure.UltimateProcessor)
	assert.Equal(t, voyage.DefaultEndpoint, disclosure.Endpoint)
	assert.Equal(t, "synthetic-epoch-v1", disclosure.Deployment)
	assert.Equal(t, "synthetic-epoch-v1", disclosure.ModelRevision)
}

func TestVoyageTextRuntimeConfigRejectsUnpinnedAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.EmbeddingRuntimeConfig)
		want   string
	}{
		{"missing epoch", func(r *config.EmbeddingRuntimeConfig) { r.DeploymentEpoch = "" }, "requires a matching deployment epoch"},
		{"epoch drift", func(r *config.EmbeddingRuntimeConfig) { r.DeploymentEpoch = "other" }, "requires a matching deployment epoch"},
		{"manifest", func(r *config.EmbeddingRuntimeConfig) { r.CapabilityManifest = "/synthetic/manifest.json" }, "no capability manifest"},
		{"revision header", func(r *config.EmbeddingRuntimeConfig) { r.ProviderRevisionHeader = "X-Revision" }, "Voyage runtime authority is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, profile := voyageTextRuntimeConfig(t)
			runtime := *profile.Runtime
			test.mutate(&runtime)
			profile.Runtime = &runtime
			cfg.EmbeddingProfiles["voyage-text"] = profile
			require.ErrorContains(t, cfg.Validate(), test.want)
		})
	}
}

// voyageTextRuntimeConfig returns a valid daemon config whose processing
// profile selects a Voyage retained-text binding with a pinned descriptor.
func voyageTextRuntimeConfig(t *testing.T) (config.Config, config.EmbeddingProfileConfig) {
	t.Helper()
	rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(rendition.Descriptor().Fingerprint)
	cfg.CredentialBindings["voyage"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_VOYAGE_TEXT_KEY"}
	modelInput := config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileCustom), CompatibilityID: "voyage/retained-text/v1",
		Document: config.EmbeddingModelInputEncoderConfig{Mode: string(document.ModelInputModeDocument), Template: "{{content}}"},
		Query:    config.EmbeddingModelInputEncoderConfig{Mode: string(document.ModelInputModeQuery), Template: "{{content}}"}}
	profile := config.EmbeddingProfileConfig{
		Activation: string(document.EmbeddingRequired), AuthorizationFingerprint: strings.Repeat("1", 64),
		CompatibilityID: modelInput.CompatibilityID, CredentialBinding: "credential:voyage", DescriptorID: voyage.EmbeddingProviderID,
		Dimensions: 256, DisclosureFingerprint: strings.Repeat("2", 64), DocumentFormatter: voyage.EmbeddingDocumentFormatterV1,
		InputKind: string(document.EmbeddingInputRenditionChunk), MaxInputTokens: 128, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: document.VectorMetricCosine, Model: "voyage-3.5", Normalization: document.VectorNormalizationUnitLength,
		QueryFormatter: voyage.EmbeddingQueryFormatterV1, ScalarEncoding: voyage.EmbeddingScalarFloat32, TrustBoundary: string(document.EmbeddingTrustHostedProvider),
		Chunk: config.EmbeddingChunkConfig{ContextFingerprint: strings.Repeat("3", 64), Formatter: "rendition-chunk/v1", MaxTokens: 128, OverlapTokens: 8,
			Tokenizer: "unicode-runes", TokenizerRevision: "v1", TruncationPolicy: string(document.TruncationPolicyReject)}, ModelInput: modelInput,
		Runtime: &config.EmbeddingRuntimeConfig{AdapterContract: voyageEmbeddingAdapter, Endpoint: voyage.DefaultEndpoint,
			ModelRevision: "synthetic-epoch-v1", DeploymentEpoch: "synthetic-epoch-v1", RequestTimeout: config.Duration(time.Second), MaxRequestBytes: 1 << 20,
			AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}, ProxyMode: "disabled", ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second), TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	cfg.EmbeddingProfiles["voyage-text"] = profile
	contract, err := cfg.EmbeddingModelInput("voyage-text")
	require.NoError(t, err)
	pinned, err := finalizeVoyageEmbeddingDescriptor(voyageRuntimeProfile(profile, contract))
	require.NoError(t, err)
	profile.DescriptorFingerprint = pinned.Descriptor.Fingerprint
	cfg.EmbeddingProfiles["voyage-text"] = profile
	processing := cfg.ProcessingProfiles["private-text"]
	processing.Embeddings = []string{"voyage-text"}
	cfg.ProcessingProfiles["private-text"] = processing
	require.NoError(t, cfg.Validate())
	return cfg, profile
}
