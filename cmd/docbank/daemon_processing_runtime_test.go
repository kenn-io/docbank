package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func TestExecutableProcessingProfilesRegistersPlaintextRendition(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	descriptor := provider.Descriptor()
	cfg := plaintextProcessingConfig(descriptor.Fingerprint)
	require.NoError(t, cfg.Validate())

	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	require.Contains(t, profiles, "private-text")
	require.NotNil(t, profiles["private-text"].RenditionProvider)
	assert.Equal(t, descriptor, profiles["private-text"].RenditionProvider.Descriptor())
	assert.Empty(t, profiles["private-text"].EmbeddingProviders)
}

func TestExecutableProcessingProfilesRegistersDoclingASR(t *testing.T) {
	cfg, descriptor := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
	require.NoError(t, cfg.Validate())

	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	profile, ok := profiles["asr"]
	require.True(t, ok)
	require.NotNil(t, profile.RenditionProvider)
	assert.Equal(t, descriptor, profile.RenditionProvider.Descriptor())
	assert.Equal(t, config.DoclingASRAdapterContract, profile.RenditionDisclosure.ImmediateProcessor)
	assert.Equal(t, descriptor.ID, profile.RenditionDisclosure.UltimateProcessor)
	assert.Equal(t, "http://127.0.0.1:5001", profile.RenditionDisclosure.Endpoint)
	assert.Equal(t, strings.Repeat("2", 64), profile.RenditionDisclosure.Deployment)
	assert.Empty(t, profile.RenditionDisclosure.Model)
	assert.Empty(t, profile.RenditionDisclosure.ModelRevision)
}

func TestExecutableProcessingProfilesDoclingASRIdentityAndReuse(t *testing.T) {
	t.Run("descriptor drift", func(t *testing.T) {
		cfg, _ := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
		profile := cfg.RenditionProfiles["asr"]
		profile.DescriptorFingerprint = strings.Repeat("0", 64)
		cfg.RenditionProfiles["asr"] = profile
		require.NoError(t, cfg.Validate())

		_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.ErrorContains(t, err, "descriptor differs from portable binding")
	})

	t.Run("identical settings share provider", func(t *testing.T) {
		cfg, descriptor := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
		cfg.RenditionProfiles["alternate"] = cfg.RenditionProfiles["asr"]
		alternateProfile := cfg.RenditionProfiles["alternate"]
		alternateProfile.Runtime = cloneRenditionRuntime(alternateProfile.Runtime)
		alternateProfile.Runtime.SPKISHA256 = []string{}
		alternateProfile.DeploymentFingerprint = strings.Repeat("a", 64)
		alternateProfile.DisclosureFingerprint = docling.ASRDisclosureFingerprint(
			descriptor, alternateProfile.Runtime.Endpoint, alternateProfile.DeploymentFingerprint)
		cfg.RenditionProfiles["alternate"] = alternateProfile
		cfg.ProcessingProfiles["alternate"] = cfg.ProcessingProfiles["asr"]
		configured := cfg.ProcessingProfiles["alternate"]
		configured.Rendition = "alternate"
		cfg.ProcessingProfiles["alternate"] = configured
		require.NoError(t, cfg.Validate())

		profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.NoError(t, err)
		assert.Same(t, profiles["asr"].RenditionProvider, profiles["alternate"].RenditionProvider)
		assert.Equal(t, strings.Repeat("a", 64), profiles["alternate"].RenditionDisclosure.Deployment)
	})

	t.Run("disclosure binds endpoint", func(t *testing.T) {
		cfg, _ := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
		profile := cfg.RenditionProfiles["asr"]
		profile.Runtime = cloneRenditionRuntime(profile.Runtime)
		profile.Runtime.Endpoint = "http://127.0.0.1:5002"
		cfg.RenditionProfiles["asr"] = profile
		require.NoError(t, cfg.Validate())

		_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.ErrorContains(t, err, "disclosure fingerprint does not bind the runtime endpoint")
	})

	t.Run("stale processing profile is rejected", func(t *testing.T) {
		cfg, _ := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
		root := t.TempDir()
		require.NoError(t, writeDaemonASRConfig(root, cfg))
		loaded := config.Default()
		_, err := toml.DecodeFile(filepath.Join(root, "config.toml"), &loaded)
		require.NoError(t, err)
		require.NoError(t, loaded.Validate())
		cfg = loaded
		providers, _, err := configureRenditionProviders(cfg)
		require.NoError(t, err)
		provider, ok := providers["asr"].(*configuredRenditionProvider)
		require.True(t, ok)
		resolved, err := cfg.ProcessingProfile("asr")
		require.NoError(t, err)
		_, fingerprints, err := document.CanonicalProfile(resolved.Document)
		require.NoError(t, err)
		_, ok = provider.allowedRenditionRequests[fingerprints.RenditionRequest]
		require.True(t, ok)

		_, err = provider.Render(t.Context(), nil, document.RenditionAuthorization{
			RenditionRequestFingerprint: strings.Repeat("0", 64),
		})
		require.ErrorIs(t, err, document.ErrRenditionAuthorizationInvalid)
		require.NoError(t, document.ValidateRenditionProviderError(err))
	})

	t.Run("missing credential mapping", func(t *testing.T) {
		cfg, _ := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
		cfg.CredentialBindings = nil

		_, _, err := configureRenditionProviders(cfg)
		require.ErrorContains(t, err, "credential binding is not configured")
	})

	t.Run("effective policy conflict", func(t *testing.T) {
		cfg, descriptor := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
		cfg.RenditionProfiles["alternate"] = cfg.RenditionProfiles["asr"]
		alternate := cfg.RenditionProfiles["alternate"]
		alternate.Runtime = cloneRenditionRuntime(alternate.Runtime)
		alternate.Runtime.Endpoint = "http://127.0.0.1:5002"
		alternate.DisclosureFingerprint = docling.ASRDisclosureFingerprint(
			descriptor, alternate.Runtime.Endpoint, alternate.DeploymentFingerprint)
		cfg.RenditionProfiles["alternate"] = alternate
		cfg.ProcessingProfiles["alternate"] = cfg.ProcessingProfiles["asr"]
		configured := cfg.ProcessingProfiles["alternate"]
		configured.Rendition = "alternate"
		cfg.ProcessingProfiles["alternate"] = configured
		require.NoError(t, cfg.Validate())

		_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
		require.ErrorContains(t, err, "conflicts with another profile's provider")
	})
}

func TestExecutableProcessingProfilesAllowsUnselectedDoclingASRRuntime(t *testing.T) {
	cfg, _ := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
	cfg.ProcessingProfiles = map[string]config.ProcessingProfileConfig{}
	require.NoError(t, cfg.Validate())

	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	assert.Empty(t, profiles)
}

func TestDoclingASRRuntimeBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*config.RenditionProfileConfig)
		want string
	}{
		{"document bytes", func(profile *config.RenditionProfileConfig) {
			profile.MaxDocumentBytes = docling.MaxDocumentBytes + 1
		}, "bounds are invalid"},
		{"response bytes", func(profile *config.RenditionProfileConfig) {
			profile.MaxResponseBytes = docling.MaxResponseBytes + 1
		}, "bounds are invalid"},
		{"transcript policy drift", func(profile *config.RenditionProfileConfig) {
			profile.MaxTranscriptChars++
		}, "descriptor differs from portable binding"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, _ := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
			profile := cfg.RenditionProfiles["asr"]
			test.set(&profile)
			cfg.RenditionProfiles["asr"] = profile
			require.NoError(t, cfg.Validate())
			_, _, err := configureRenditionProviders(cfg)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestDoclingASRDescriptorIsIndependentOfProcessingLimits(t *testing.T) {
	cfg, descriptor := doclingASRProcessingConfig(t, "http://127.0.0.1:5001")
	second := cfg.ProcessingProfiles["asr"]
	second.MaxDocumentChars--
	cfg.ProcessingProfiles["second"] = second
	require.NoError(t, cfg.Validate())
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	assert.Equal(t, descriptor, profiles["second"].RenditionProvider.Descriptor())
	assert.Same(t, profiles["asr"].RenditionProvider, profiles["second"].RenditionProvider)
}

func cloneRenditionRuntime(runtime *config.RenditionRuntimeConfig) *config.RenditionRuntimeConfig {
	cloned := *runtime
	cloned.AllowedCIDRs = slices.Clone(runtime.AllowedCIDRs)
	cloned.SPKISHA256 = slices.Clone(runtime.SPKISHA256)
	return &cloned
}

func doclingASRProcessingConfig(t *testing.T, endpoint string) (config.Config, document.RenditionDescriptor) {
	t.Helper()
	const maxDocumentChars = 100_000
	policyFingerprint, err := docling.ASRPolicyFingerprint(maxDocumentChars)
	require.NoError(t, err)
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "docling.serve-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: policyFingerprint, TrustBoundary: document.RenditionTrustOperatorNetwork,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
		ArtifactRoles:     []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
	})
	require.NoError(t, err)
	cfg := config.Default()
	cfg.CredentialBindings["docling"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_DOCLING_KEY"}
	cfg.RenditionProfiles["asr"] = config.RenditionProfileConfig{
		AdapterContract: config.DoclingASRAdapterContract, AuthorizationFingerprint: strings.Repeat("1", 64),
		CredentialBinding: "credential:docling", DeploymentFingerprint: strings.Repeat("2", 64),
		DescriptorID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		DisclosureFingerprint: docling.ASRDisclosureFingerprint(descriptor, endpoint, strings.Repeat("2", 64)), MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, MaxUnits: 100, MaxTranscriptChars: maxDocumentChars,
		RequestedArtifacts: []string{string(document.EvidenceArtifactTranscript)},
		TrustBoundary:      string(document.RenditionTrustOperatorNetwork), UploadOptionsFingerprint: strings.Repeat("4", 64),
		Runtime: &config.RenditionRuntimeConfig{
			Endpoint: endpoint, RequestTimeout: config.Duration(time.Second), TotalTimeout: config.Duration(10 * time.Second),
			PollInterval: config.Duration(time.Millisecond), MaxPollAttempts: 4,
			AllowedCIDRs: []string{"127.0.0.0/8"}, ProxyMode: "disabled",
			ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second),
			TLSHandshakeTimeout: config.Duration(time.Second),
		},
	}
	cfg.RetrievalProfiles["lexical"] = config.RetrievalProfileConfig{LexicalLimit: 20, VectorLimit: 20}
	cfg.ProcessingProfiles["asr"] = config.ProcessingProfileConfig{
		Rendition: "asr", Retrieval: "lexical", AttachmentPolicyFingerprint: strings.Repeat("5", 64),
		CompletenessFingerprint: strings.Repeat("6", 64), ConsentFingerprint: strings.Repeat("7", 64),
		LexicalSegmenterFingerprint: strings.Repeat("8", 64), MaxDocumentChars: maxDocumentChars,
		MaxSegmentRunes: 2000, MaxUnitRunes: 100000, NormalizerFingerprint: strings.Repeat("9", 64),
		RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, SanitizerFingerprint: strings.Repeat("a", 64), TrustBoundary: "vault-primary",
	}
	return cfg, descriptor
}

func TestExecutableProcessingProfilesRejectsDriftedPlaintextDescriptor(t *testing.T) {
	cfg := plaintextProcessingConfig(strings.Repeat("0", 64))
	require.NoError(t, cfg.Validate())

	_, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.ErrorContains(t, err, "descriptor differs from portable binding")
}

func TestExecutableProcessingProfilesRegistersPinnedRenditionChunkTokenizer(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(provider.Descriptor().Fingerprint)
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileNomic,
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic.embedding-v1", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: strings.Repeat("e", 64), TrustBoundary: document.EmbeddingTrustOperatorNetwork,
		Model: "synthetic-model", ModelRevision: "v1", Dimension: 2, Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationNone, ScalarEncoding: "float32",
		DocumentFormatter: "synthetic/document-v1", QueryFormatter: "synthetic/query-v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{contract.Document.Mode},
	})
	require.NoError(t, err)
	cfg.EmbeddingProfiles["semantic"] = config.EmbeddingProfileConfig{
		Activation: string(document.EmbeddingOptional), AuthorizationFingerprint: strings.Repeat("1", 64),
		CompatibilityID: contract.CompatibilityID, CredentialBinding: "credential:semantic",
		DescriptorID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, Dimensions: descriptor.Dimension,
		DisclosureFingerprint: strings.Repeat("2", 64), DocumentFormatter: descriptor.DocumentFormatter,
		InputKind: string(document.EmbeddingInputRenditionChunk), MaxBatchItems: 8, MaxInputBytes: 1 << 20,
		MaxInputTokens: 128, MaxResponseBytes: 1 << 20, Metric: descriptor.Metric, Model: descriptor.Model,
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
		Chunk: config.EmbeddingChunkConfig{ContextFingerprint: strings.Repeat("3", 64), Formatter: "synthetic/v1",
			MaxTokens: 128, OverlapTokens: 8, Tokenizer: "unicode-runes", TokenizerRevision: "v1",
			TruncationPolicy: string(document.TruncationPolicyReject)},
		ModelInput: config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)},
		Runtime: &config.EmbeddingRuntimeConfig{AdapterContract: openAIEmbeddingAdapter,
			Endpoint: "http://embed.internal:11434", ModelRevision: "v1", DeploymentEpoch: "v1",
			RequestTimeout: config.Duration(time.Second), MaxRequestBytes: 1 << 20,
			AllowedCIDRs: []string{"10.0.0.0/8"}, ProxyMode: "disabled", ConnectTimeout: config.Duration(time.Second),
			KeepAlive: config.Duration(time.Second), TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	cfg.CredentialBindings["semantic"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_SEMANTIC_KEY"}
	cfg.ProcessingProfiles["private-text"] = config.ProcessingProfileConfig{
		Rendition: "plaintext", Embeddings: []string{"semantic"}, Retrieval: "lexical",
		AttachmentPolicyFingerprint: strings.Repeat("5", 64), CompletenessFingerprint: strings.Repeat("6", 64),
		ConsentFingerprint: strings.Repeat("7", 64), LexicalSegmenterFingerprint: strings.Repeat("8", 64),
		MaxSegmentRunes: 2000, MaxUnitRunes: 100000, MaxDocumentChars: 1000000, NormalizerFingerprint: strings.Repeat("9", 64),
		RetainSanitizedMarkdown: true, SanitizerFingerprint: strings.Repeat("a", 64), TrustBoundary: "vault-primary",
	}
	require.NoError(t, cfg.Validate())
	bundle := embeddingRuntimeBundle{
		providers: map[string]document.EmbeddingProvider{"semantic": inertEmbeddingProvider{descriptor: descriptor}},
		classifiers: map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration){
			"semantic": func(error) (processing.EmbeddingProviderFailure, time.Duration) {
				return processing.EmbeddingProviderPermanent, 0
			},
		},
	}

	profiles, err := executableProcessingProfiles(cfg, bundle)
	require.NoError(t, err)
	require.Contains(t, profiles, "private-text")
	assert.Equal(t, unicodeRuneSpec,
		profiles["private-text"].Tokenizers["semantic"].Identity().Name+"@"+
			profiles["private-text"].Tokenizers["semantic"].Identity().Revision)
	disclosure := profiles["private-text"].EmbeddingDisclosures["semantic"]
	assert.Equal(t, openAIEmbeddingAdapter, disclosure.ImmediateProcessor)
	assert.Equal(t, descriptor.ID, disclosure.UltimateProcessor)
	assert.Equal(t, "http://embed.internal:11434", disclosure.Endpoint)
	assert.Equal(t, "v1", disclosure.Deployment)
}

type inertEmbeddingProvider struct{ descriptor document.EmbeddingDescriptor }

func (provider inertEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}
func (inertEmbeddingProvider) Embed(context.Context, []document.EmbeddingInput,
	document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	return document.EmbeddingResult{}, nil
}

func plaintextProcessingConfig(descriptorFingerprint string) config.Config {
	cfg := config.Default()
	cfg.RenditionProfiles["plaintext"] = config.RenditionProfileConfig{
		AdapterContract: plaintextRenditionAdapter, AuthorizationFingerprint: strings.Repeat("1", 64),
		CredentialBinding: "credential:none", DeploymentFingerprint: strings.Repeat("2", 64),
		DescriptorID: "plaintext.in-process-v1", DescriptorFingerprint: descriptorFingerprint,
		DisclosureFingerprint: strings.Repeat("3", 64), MaxDocumentBytes: plaintext.MaxDocumentBytes,
		MaxResponseBytes: plaintext.MaxDocumentBytes, MaxUnits: 1,
		RequestedArtifacts: []string{string(document.EvidenceArtifactStructured)},
		TrustBoundary:      string(document.RenditionTrustLocalProcess), UploadOptionsFingerprint: strings.Repeat("4", 64),
	}
	cfg.RetrievalProfiles["lexical"] = config.RetrievalProfileConfig{LexicalLimit: 20, VectorLimit: 20}
	cfg.ProcessingProfiles["private-text"] = config.ProcessingProfileConfig{
		Rendition: "plaintext", Retrieval: "lexical", AttachmentPolicyFingerprint: strings.Repeat("5", 64),
		CompletenessFingerprint: strings.Repeat("6", 64), ConsentFingerprint: strings.Repeat("7", 64),
		LexicalSegmenterFingerprint: strings.Repeat("8", 64), MaxSegmentRunes: 2000, MaxUnitRunes: 100000, MaxDocumentChars: 1000000,
		NormalizerFingerprint: strings.Repeat("9", 64), RetainSanitizedMarkdown: true,
		SanitizerFingerprint: strings.Repeat("a", 64), TrustBoundary: "vault-primary",
	}
	return cfg
}

func TestDaemonStartsConfiguredRenditionWorker(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(provider.Descriptor().Fingerprint)
	cfg.Server.APIKey = "synthetic-processing-key"
	var encoded bytes.Buffer
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{
		"server":             map[string]string{"api_key": cfg.Server.APIKey},
		"rendition_profiles": cfg.RenditionProfiles, "processing_profiles": cfg.ProcessingProfiles,
		"retrieval_profiles": cfg.RetrievalProfiles,
	}))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), encoded.Bytes(), 0o600))
	t.Setenv("DOCBANK_HOME", root)
	startServe(t)
	runtime := waitForDaemon(t, root)
	c := daemonconn.New("http://"+runtime.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	out, err := runCLI(t, "formats", "--json")
	require.NoError(t, err)
	var coverage api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(out), &coverage))
	// The daemon also registers its two built-in supplied media providers.
	assert.Len(t, coverage.GeneratedBy.BoundProviders, 3)
	assert.Contains(t, coverage.GeneratedBy.BoundProviders, provider.Descriptor().Fingerprint)

	jobs, err := c.API().ListJobs(t.Context())

	require.NoError(t, err)
	for _, job := range jobs.Items {
		if job.Name == "process:renditions" {
			require.Equal(t, "running", job.Status)
			return
		}
	}
	t.Fatal("configured rendition worker did not start")
}
