package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/openaiembed"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
)

func TestStartEmbeddingWorkerIfReadySkipsMissingBindings(t *testing.T) {
	starter := &fakeEmbeddingJobStarter{}
	built := false
	err := startEmbeddingWorkerIfReady(starter, embeddingReady(false), func() (embeddingJobRunner, error) {
		built = true
		return embeddingRunnerFunc(func(context.Context) error { return nil }), nil
	})
	require.NoError(t, err)
	assert.False(t, built)
	assert.Empty(t, starter.name)
}

func TestStartEmbeddingWorkerIfReadyUsesSupervisorCancellation(t *testing.T) {
	starter := &fakeEmbeddingJobStarter{}
	err := startEmbeddingWorkerIfReady(starter, embeddingReady(true), func() (embeddingJobRunner, error) {
		return embeddingRunnerFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}), nil
	})
	require.NoError(t, err)
	assert.Equal(t, "process:embeddings", starter.name)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, starter.run(ctx), context.Canceled)

	want := errors.New("synthetic configuration failure")
	err = startEmbeddingWorkerIfReady(&fakeEmbeddingJobStarter{}, embeddingReady(true), func() (embeddingJobRunner, error) {
		return nil, want
	})
	require.ErrorIs(t, err, want)
}

func TestConfigureEmbeddingRuntimesFailsWhenNamedCredentialEnvironmentIsMissing(t *testing.T) {
	t.Setenv("DOCBANK_TEST_MISSING_EMBEDDING_KEY", "")
	cfg := config.Default()
	cfg.CredentialBindings["semantic"] = config.CredentialBindingConfig{
		EnvironmentVariable: "DOCBANK_TEST_MISSING_EMBEDDING_KEY",
	}
	cfg.EmbeddingProfiles["semantic"] = config.EmbeddingProfileConfig{
		CredentialBinding: "credential:semantic",
		Runtime:           &config.EmbeddingRuntimeConfig{AdapterContract: openAIEmbeddingAdapter},
	}
	_, err := configureEmbeddingRuntimes(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.ErrorContains(t, err, "environment variable is missing")
}

func TestConfigureEmbeddingRuntimesRegistersSyntheticLoopbackOpenAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	t.Setenv("DOCBANK_TEST_EMBEDDING_KEY", "synthetic-secret")
	cfg := config.Default()
	cfg.CredentialBindings["semantic"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_EMBEDDING_KEY"}
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	require.NoError(t, err)
	profile := config.EmbeddingProfileConfig{
		Activation: string(document.EmbeddingRequired), AuthorizationFingerprint: strings.Repeat("1", 64),
		CompatibilityID: contract.CompatibilityID, CredentialBinding: "credential:semantic",
		DescriptorID: openaiembed.ProviderID, Dimensions: 2, DisclosureFingerprint: strings.Repeat("2", 64),
		DocumentFormatter: openaiembed.DocumentFormatterV1, InputKind: string(document.EmbeddingInputRenditionChunk),
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: document.VectorMetricCosine, Model: "synthetic-model", Normalization: document.VectorNormalizationNone,
		QueryFormatter: openaiembed.QueryFormatterV1, ScalarEncoding: openaiembed.ScalarEncodingFloat32,
		TrustBoundary: string(document.EmbeddingTrustOperatorNetwork),
		Chunk: config.EmbeddingChunkConfig{ContextFingerprint: strings.Repeat("3", 64), Formatter: "synthetic/v1",
			MaxTokens: 128, OverlapTokens: 8, Tokenizer: "synthetic@v1", TruncationPolicy: string(document.TruncationPolicyReject)},
		ModelInput: config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)},
		Runtime: &config.EmbeddingRuntimeConfig{AdapterContract: openAIEmbeddingAdapter, Endpoint: server.URL,
			ModelRevision: "deployment-v1", DeploymentEpoch: "deployment-v1", RequestTimeout: config.Duration(time.Second),
			MaxRequestBytes: 1 << 20, MaxRetries: 1, AllowedCIDRs: []string{"127.0.0.0/8"}, ProxyMode: "disabled",
			ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second),
			TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	descriptor := configuredEmbeddingDescriptor(profile, contract)
	final, _, err := finalizeOpenAIEmbeddingDescriptor(openaiembed.Profile{Origin: server.URL, Descriptor: descriptor,
		ModelInput: contract, SecretBinding: profile.CredentialBinding, DeploymentEpoch: profile.Runtime.DeploymentEpoch,
		RequestTimeout: profile.Runtime.RequestTimeout.Std(), MaxBatchItems: profile.MaxBatchItems,
		MaxInputBytes: profile.MaxInputBytes, MaxRequestBytes: profile.Runtime.MaxRequestBytes,
		MaxResponseBytes: profile.MaxResponseBytes, EgressPolicy: configuredEmbeddingEgress(*profile.Runtime)})
	require.NoError(t, err)
	profile.DescriptorFingerprint = final.Fingerprint
	cfg.EmbeddingProfiles["semantic"] = profile
	require.NoError(t, cfg.Validate())

	registry, err := configureEmbeddingRuntimes(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	assert.True(t, registry.Ready())
	assert.Equal(t, []string{final.Fingerprint}, registry.Fingerprints())
	classification, _ := classifyOpenAIEmbeddingError(fmt.Errorf("%w: local request envelope", openaiembed.ErrCapacityResponse))
	assert.Equal(t, processing.EmbeddingProviderCapacity, classification)
}

func TestConfigureEmbeddingRuntimesRegistersCapabilityAttestedVoyageOriginal(t *testing.T) {
	t.Setenv("DOCBANK_TEST_VOYAGE_KEY", "synthetic-secret")
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: voyage.DefaultModel, Dimension: voyage.DefaultDimension,
		Media: media.Policy{MaxBytes: 1 << 20, AllowStill: true, AllowVideo: true}, MaxBatchItems: 8,
		MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityImagePNG)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, voyage.EncodeCapabilityManifest(&encoded, manifest))
	manifestPath := filepath.Join(t.TempDir(), "voyage-capabilities.json")
	require.NoError(t, os.WriteFile(manifestPath, encoded.Bytes(), 0o600))
	revision, err := voyage.DirectFileModelRevision(policy, manifest)
	require.NoError(t, err)
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "voyage/synthetic-direct/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	profile := config.EmbeddingProfileConfig{
		Activation: string(document.EmbeddingRequired), AuthorizationFingerprint: strings.Repeat("1", 64),
		CompatibilityID: contract.CompatibilityID, CredentialBinding: "credential:voyage",
		DescriptorID: voyage.EmbeddingProviderID, Dimensions: voyage.DefaultDimension,
		DisclosureFingerprint: strings.Repeat("2", 64), DocumentFormatter: voyage.EmbeddingDocumentFormatterV1,
		InputKind: string(document.EmbeddingInputOriginalFile), MaxBatchItems: 8, MaxInputBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, Metric: document.VectorMetricCosine, Model: voyage.DefaultModel,
		Normalization: document.VectorNormalizationUnitLength, QueryFormatter: voyage.EmbeddingQueryFormatterV1,
		ScalarEncoding: voyage.EmbeddingScalarFloat32, TrustBoundary: string(document.EmbeddingTrustHostedProvider),
		ModelInput: config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileCustom),
			CompatibilityID: contract.CompatibilityID,
			Document:        config.EmbeddingModelInputEncoderConfig{Mode: string(document.ModelInputModeDocument), Template: "document: {{content}}"},
			Query:           config.EmbeddingModelInputEncoderConfig{Mode: string(document.ModelInputModeQuery), Template: "query: {{content}}"}},
		Runtime: &config.EmbeddingRuntimeConfig{AdapterContract: voyageEmbeddingAdapter, Endpoint: voyage.DefaultEndpoint,
			ModelRevision: revision, CapabilityManifest: manifestPath, RequestTimeout: config.Duration(time.Second),
			MaxRequestBytes: 1 << 20, MaxRetries: 1, AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}, ProxyMode: "disabled",
			ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second),
			TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	secrets := environmentEmbeddingSecrets{variables: map[string]string{"credential:voyage": "DOCBANK_TEST_VOYAGE_KEY"}}
	_, final, err := configuredVoyageProvider(profile, contract, secrets)
	require.NoError(t, err)
	profile.DescriptorFingerprint = final.Fingerprint
	cfg := config.Default()
	cfg.CredentialBindings["voyage"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_VOYAGE_KEY"}
	cfg.EmbeddingProfiles["voyage"] = profile
	require.NoError(t, cfg.Validate())

	registry, err := configureEmbeddingRuntimes(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, []string{final.Fingerprint}, registry.Fingerprints())
}

func TestRecoverEmbeddingRuntimeSpoolRemovesOnlyAbandonedUploadState(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".docbank-upload-synthetic")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "source"), []byte("partial"), 0o600))
	unrelated := filepath.Join(root, "blob-partial")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))

	require.NoError(t, recoverEmbeddingRuntimeSpool(t.Context(), root))
	_, err := os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(unrelated)
	require.NoError(t, err)
}

type unavailableEmbeddingBlobs struct{}

func (unavailableEmbeddingBlobs) OpenContext(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, errors.New("unexpected blob read")
}

type embeddingReady bool

func (ready embeddingReady) Ready() bool { return bool(ready) }

type fakeEmbeddingJobStarter struct {
	name string
	run  func(context.Context) error
}

func (starter *fakeEmbeddingJobStarter) Start(name string, run func(context.Context) error) error {
	starter.name, starter.run = name, run
	return nil
}

type embeddingRunnerFunc func(context.Context) error

func (run embeddingRunnerFunc) Run(ctx context.Context) error { return run(ctx) }
