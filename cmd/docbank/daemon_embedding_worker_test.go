package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/openaicompat"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestDaemonRecoversSemanticSpaceEmbeddingJob(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "synthetic expired credential", http.StatusUnauthorized)
			return
		}
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		data := make([]map[string]any, len(body.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 2}, "object": "embedding"}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data, "model": "synthetic-model", "object": "list"}))
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCBANK_TEST_EMBEDDING_KEY", "synthetic-secret")
	cfg, _ := syntheticLoopbackOpenAIConfig(t, server.URL, "DOCBANK_TEST_EMBEDDING_KEY")
	fingerprint, err := plaintextProviderForDaemonTest()
	require.NoError(t, err)
	plain := plaintextProcessingConfig(fingerprint)
	cfg.RenditionProfiles, cfg.RetrievalProfiles, cfg.ProcessingProfiles = plain.RenditionProfiles, plain.RetrievalProfiles, plain.ProcessingProfiles
	profile := cfg.ProcessingProfiles["private-text"]
	profile.Embeddings = []string{"semantic"}
	cfg.ProcessingProfiles["private-text"] = profile
	embedding := cfg.EmbeddingProfiles["semantic"]
	embedding.Chunk.Tokenizer = "unicode-runes"
	cfg.EmbeddingProfiles["semantic"] = embedding
	require.NoError(t, cfg.Validate())
	root := t.TempDir()
	layout := home.Layout{Root: root}
	require.NoError(t, layout.Ensure())
	func() {
		catalog, err := store.Open(layout.DBPath())
		require.NoError(t, err)
		defer func() { require.NoError(t, catalog.Close()) }()
		blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
		require.NoError(t, err)
		defer func() { require.NoError(t, blobs.Close()) }()
		bundle, err := configureEmbeddingRuntimeBundle(cfg, blobs, layout.BlobTmpDir())
		require.NoError(t, err)
		profiles, err := executableProcessingProfiles(cfg, bundle)
		require.NoError(t, err)
		service, err := processing.NewService(processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), Profiles: profiles, Principal: "daemon:operator", Scope: "document-processing", SpoolDirectory: layout.BlobTmpDir(), Lifecycle: t.Context()})
		require.NoError(t, err)
		defer func() { require.NoError(t, service.Shutdown(t.Context())) }()
		receipt, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader("a selected document"))
		require.NoError(t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "selected.txt", receipt.Hash, receipt.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize})
		require.NoError(t, err)
		selector := processing.Selector{Profile: "private-text", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
		plan, err := service.Plan(t.Context(), selector)
		require.NoError(t, err)
		job, err := service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
		require.Len(t, job.EmbeddingJobIDs, 1)
		failed, err := catalog.EmbeddingJobByID(t.Context(), job.EmbeddingJobIDs[0])
		require.NoError(t, err)
		require.Equal(t, "failed", failed.State)
	}()
	require.EqualValues(t, 1, calls.Load())
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	var encoded bytes.Buffer
	runtime := embedding.Runtime
	embedding.Runtime = nil
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{"embedding_profiles": map[string]any{"semantic": embedding}}))
	encoded.WriteString("[embedding_profiles.semantic.runtime]\n")
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{
		"adapter_contract": runtime.AdapterContract, "endpoint": runtime.Endpoint, "model_revision": runtime.ModelRevision, "deployment_epoch": runtime.DeploymentEpoch,
		"request_timeout": runtime.RequestTimeout.Std().String(), "max_request_bytes": runtime.MaxRequestBytes, "allowed_cidrs": runtime.AllowedCIDRs, "proxy_mode": runtime.ProxyMode,
		"connect_timeout": runtime.ConnectTimeout.Std().String(), "keep_alive": runtime.KeepAlive.Std().String(), "tls_handshake_timeout": runtime.TLSHandshakeTimeout.Std().String(),
	}))
	file, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.Write(encoded.Bytes())
	require.NoError(t, err)
	require.NoError(t, file.Close())
	t.Setenv("DOCBANK_HOME", root)
	startServe(t)
	waitForDaemon(t, root)
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 5*time.Second, 25*time.Millisecond, "background recovery must use the service-selected semantic space")
}

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

func TestConfigureEmbeddingRuntimesDefersMissingCredentialUntilSelectedOperation(t *testing.T) {
	serverRequests := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		serverRequests.Add(1)
	}))
	t.Cleanup(server.Close)
	const environmentName = "DOCBANK_TEST_MISSING_EMBEDDING_KEY"
	t.Setenv(environmentName, "")
	cfg, descriptor := syntheticLoopbackOpenAIConfig(t, server.URL, environmentName)

	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err, "an unused hosted profile must not require its secret during daemon startup")
	provider := bundle.providers["semantic"]
	require.NotNil(t, provider)
	_, err = provider.Embed(t.Context(), []document.EmbeddingInput{{
		Key: "document-1", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "passage",
	}}, document.EmbeddingAuthorization{ProviderID: descriptor.ID,
		DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20})
	require.ErrorIs(t, err, openaicompat.ErrUnauthorized)
	assert.Zero(t, serverRequests.Load(), "missing operation-time credentials must fail before provider transport")
	for _, private := range []string{"credential:semantic", environmentName, "synthetic-secret"} {
		assert.NotContains(t, err.Error(), private)
	}
}

func TestConfigureEmbeddingRuntimesRegistersSyntheticLoopbackOpenAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	t.Setenv("DOCBANK_TEST_EMBEDDING_KEY", "synthetic-secret")
	cfg, final := syntheticLoopbackOpenAIConfig(t, server.URL, "DOCBANK_TEST_EMBEDDING_KEY")

	duplicate := cfg.EmbeddingProfiles["semantic"]
	duplicate.Chunk.MaxTokens = 64
	cfg.EmbeddingProfiles["alternate-chunks"] = duplicate
	require.NoError(t, cfg.Validate())
	t.Setenv("DOCBANK_TEST_EMBEDDING_KEY", "")
	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	assert.True(t, bundle.registry.Ready())
	assert.Equal(t, []string{final.Fingerprint}, bundle.registry.Fingerprints())
	classification, _ := classifyOpenAIEmbeddingError(fmt.Errorf("%w: local request envelope", openaicompat.ErrCapacityResponse))
	assert.Equal(t, processing.EmbeddingProviderCapacity, classification)
}

func syntheticLoopbackOpenAIConfig(t *testing.T, endpoint, environmentName string) (config.Config, document.EmbeddingDescriptor) {
	t.Helper()
	cfg := config.Default()
	cfg.CredentialBindings["semantic"] = config.CredentialBindingConfig{EnvironmentVariable: environmentName}
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	require.NoError(t, err)
	profile := config.EmbeddingProfileConfig{
		Activation: string(document.EmbeddingRequired), AuthorizationFingerprint: strings.Repeat("1", 64),
		CompatibilityID: contract.CompatibilityID, CredentialBinding: "credential:semantic",
		DescriptorID: openaicompat.ProviderID, Dimensions: 2, DisclosureFingerprint: strings.Repeat("2", 64),
		DocumentFormatter: openaicompat.DocumentFormatterV1, InputKind: string(document.EmbeddingInputRenditionChunk),
		MaxInputTokens: 128, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: document.VectorMetricCosine, Model: "synthetic-model", Normalization: document.VectorNormalizationNone,
		QueryFormatter: openaicompat.QueryFormatterV1, ScalarEncoding: openaicompat.ScalarEncodingFloat32,
		TrustBoundary: string(document.EmbeddingTrustOperatorNetwork),
		Chunk: config.EmbeddingChunkConfig{ContextFingerprint: strings.Repeat("3", 64), Formatter: "synthetic/v1",
			MaxTokens: 128, OverlapTokens: 8, Tokenizer: "synthetic", TokenizerRevision: "v1", TruncationPolicy: string(document.TruncationPolicyReject)},
		ModelInput: config.EmbeddingModelInputConfig{Profile: string(document.ModelInputProfileNomic)},
		Runtime: &config.EmbeddingRuntimeConfig{AdapterContract: openAIEmbeddingAdapter, Endpoint: endpoint,
			ModelRevision: "deployment-v1", DeploymentEpoch: "deployment-v1", RequestTimeout: config.Duration(time.Second),
			MaxRequestBytes: 1 << 20, AllowedCIDRs: []string{"127.0.0.0/8"}, ProxyMode: "disabled",
			ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second),
			TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	descriptor := configuredEmbeddingDescriptor(profile, contract)
	final, _, err := finalizeOpenAIEmbeddingDescriptor(openaicompat.Profile{Origin: endpoint, Descriptor: descriptor,
		ModelInput: contract, SecretBinding: profile.CredentialBinding, DeploymentEpoch: profile.Runtime.DeploymentEpoch,
		RequestTimeout: profile.Runtime.RequestTimeout.Std(), MaxBatchItems: profile.MaxBatchItems,
		MaxInputBytes: profile.MaxInputBytes, MaxRequestBytes: profile.Runtime.MaxRequestBytes,
		MaxResponseBytes: profile.MaxResponseBytes, EgressPolicy: providerEgressPolicy(profile.Runtime.ProviderEgressConfig)})
	require.NoError(t, err)
	profile.DescriptorFingerprint = final.Fingerprint
	cfg.EmbeddingProfiles["semantic"] = profile
	require.NoError(t, cfg.Validate())
	return cfg, final
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
			MaxRequestBytes: 1 << 20, AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}, ProxyMode: "disabled",
			ConnectTimeout: config.Duration(time.Second), KeepAlive: config.Duration(time.Second),
			TLSHandshakeTimeout: config.Duration(time.Second)},
	}
	secrets := environmentCredentialSecrets{variables: map[string]string{"credential:voyage": "DOCBANK_TEST_VOYAGE_KEY"}}
	_, final, err := configuredVoyageProvider(profile, contract, secrets)
	require.NoError(t, err)
	profile.DescriptorFingerprint = final.Fingerprint
	cfg := config.Default()
	cfg.CredentialBindings["voyage"] = config.CredentialBindingConfig{EnvironmentVariable: "DOCBANK_TEST_VOYAGE_KEY"}
	cfg.EmbeddingProfiles["voyage"] = profile
	require.NoError(t, cfg.Validate())

	bundle, err := configureEmbeddingRuntimeBundle(cfg, unavailableEmbeddingBlobs{}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, []string{final.Fingerprint}, bundle.registry.Fingerprints())
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

func TestEmbeddingRuntimeClassifiesAuthorizationFailures(t *testing.T) {
	for _, err := range []error{openaicompat.ErrUnauthorized, &openaicompat.ProviderError{Kind: openaicompat.ErrUnauthorized, StatusCode: http.StatusUnauthorized}} {
		classification, _ := classifyOpenAIEmbeddingError(err)
		require.Equal(t, processing.EmbeddingProviderAuthorization, classification)
	}
	for _, err := range []error{voyage.ErrUnauthorized, &voyage.ProviderError{Kind: voyage.ErrUnauthorized, StatusCode: http.StatusForbidden}} {
		classification, _ := classifyVoyageEmbeddingError(err)
		require.Equal(t, processing.EmbeddingProviderAuthorization, classification)
	}
}

func TestEmbeddingRuntimeClassifiesMalformedOpenAIResponses(t *testing.T) {
	for _, err := range []error{openaicompat.ErrMalformedResponse, fmt.Errorf("schema mismatch: %w", openaicompat.ErrMalformedResponse)} {
		classification, delay := classifyOpenAIEmbeddingError(err)
		require.Equal(t, processing.EmbeddingProviderInvalidResponse, classification)
		require.Zero(t, delay)
	}
}

func TestEmbeddingRuntimeClassifiesMalformedVoyageResponses(t *testing.T) {
	for _, err := range []error{voyage.ErrMalformedResponse, &voyage.ProviderError{Kind: voyage.ErrMalformedResponse}} {
		kind, delay := classifyVoyageEmbeddingError(err)
		require.Equal(t, processing.EmbeddingProviderInvalidResponse, kind)
		require.Zero(t, delay)
	}
}
