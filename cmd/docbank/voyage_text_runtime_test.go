package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/internal/config"
)

func TestConfigureVoyageRetainedTextRuntime(t *testing.T) {
	cfg := config.Default()
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
			Tokenizer: "synthetic", TokenizerRevision: "v1", TruncationPolicy: string(document.TruncationPolicyReject)}, ModelInput: modelInput,
		Runtime: &config.EmbeddingRuntimeConfig{AdapterContract: voyageEmbeddingAdapter, Endpoint: voyage.DefaultEndpoint,
			ModelRevision: "synthetic-epoch-v1", DeploymentEpoch: "synthetic-epoch-v1", RequestTimeout: config.Duration(time.Second), MaxRequestBytes: 1 << 20,
			AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}, ProxyMode: "disabled", ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second), TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	cfg.EmbeddingProfiles["voyage-text"] = profile
	contract, err := cfg.EmbeddingModelInput("voyage-text")
	require.NoError(t, err)
	pinned, err := finalizeVoyageEmbeddingDescriptor(voyageTextRuntimeProfile(profile, contract))
	require.NoError(t, err)
	profile.DescriptorFingerprint = pinned.Descriptor.Fingerprint
	cfg.EmbeddingProfiles["voyage-text"] = profile
	require.NoError(t, cfg.Validate())
	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	descriptor := bundle.providers["voyage-text"].Descriptor()
	assert.True(t, descriptor.SupportsTextQuery)
	assert.Equal(t, pinned.Descriptor.Fingerprint, descriptor.Fingerprint)
	// Validation does not require a capability manifest or read credentials.
	for name, mutate := range map[string]func(*config.EmbeddingRuntimeConfig){
		"missing epoch":   func(r *config.EmbeddingRuntimeConfig) { r.DeploymentEpoch = "" },
		"epoch drift":     func(r *config.EmbeddingRuntimeConfig) { r.DeploymentEpoch = "other" },
		"manifest":        func(r *config.EmbeddingRuntimeConfig) { r.CapabilityManifest = "/synthetic/manifest.json" },
		"revision header": func(r *config.EmbeddingRuntimeConfig) { r.ProviderRevisionHeader = "X-Revision" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := profile
			runtime := *profile.Runtime
			mutate(&runtime)
			changed.Runtime = &runtime
			cfg.EmbeddingProfiles["voyage-text"] = changed
			require.Error(t, cfg.Validate())
		})
	}
}
