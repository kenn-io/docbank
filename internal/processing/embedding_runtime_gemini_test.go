package processing

import (
	"context"
	"errors"
	"image/color"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/geminiembed"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/store"
)

func TestGeminiEmbeddingInspectionPolicyUsesScopedAuthorityAndSemanticLimits(t *testing.T) {
	client, descriptor, disclosure, _ := newGeminiPolicyClient(t, 4096, workerHash("capability"))
	record, binding := geminiPolicyProfile(t, descriptor, disclosure, 8192)
	work := fixtureGeminiPolicyWork(mediatest.PNG(2, 2, color.White), "synthetic.png", "image/png", record, binding, descriptor)

	policy, err := geminiEmbeddingInspectionPolicy(work, client)
	require.NoError(t, err)
	assert.Equal(t, workerHash("capability"), policy.ProfileFingerprint)
	assert.Equal(t, descriptor.Fingerprint, policy.DescriptorFingerprint)
	assert.Equal(t, binding.DisclosureFingerprint, policy.DisclosureFingerprint)
	assert.Equal(t, int64(4096), policy.MaxSourceBytes)
	assert.Equal(t, int64(4096), policy.MaxExpandedBytes)
	assert.Equal(t, int64(4096), policy.MaxEntryBytes)
	assert.Equal(t, int64(6), policy.MaxPages)
	assert.Equal(t, int64(10_000), policy.MaxFrames)
	assert.Equal(t, int64(120_000), policy.MaxDurationMS)
	assert.Equal(t, work.SourceFilename, policy.Filename)
	assert.Equal(t, work.SourceMediaType, policy.DeclaredMediaType)

	work.SourceFilename, work.SourceMediaType = "synthetic.mp3", "audio/mpeg; synthetic=1"
	policy, err = geminiEmbeddingInspectionPolicy(work, client)
	require.NoError(t, err)
	assert.Equal(t, int64(180_000), policy.MaxDurationMS)
}

func TestGeminiEmbeddingRuntimeRejectsAuthorityDriftBeforeSourceOpen(t *testing.T) {
	client, descriptor, disclosure, secrets := newGeminiPolicyClient(t, 4096, workerHash("capability"))
	record, binding := geminiPolicyProfile(t, descriptor, disclosure, 4096)
	data := mediatest.PNG(2, 2, color.White)
	base := fixtureGeminiPolicyWork(data, "synthetic.png", "image/png", record, binding, descriptor)
	tests := []struct {
		name   string
		mutate func(*EmbeddingWork)
	}{
		{"empty full profile", func(work *EmbeddingWork) { work.ProcessingProfile.CanonicalProfile = nil }},
		{"oversized full profile", func(work *EmbeddingWork) {
			work.ProcessingProfile.CanonicalProfile = []byte(strings.Repeat("x", maxRuntimeProcessingProfileBytes+1))
		}},
		{"full profile fingerprint transplant", func(work *EmbeddingWork) {
			work.ProcessingProfile.Fingerprint = workerHash("other-profile")
		}},
		{"binding transplant", func(work *EmbeddingWork) { work.Binding.MaxBatchItems++ }},
		{"descriptor transplant", func(work *EmbeddingWork) { work.Descriptor.Fingerprint = workerHash("other-descriptor") }},
		{"byte capacity plus one", func(work *EmbeddingWork) { work.SourceBytes = 4097 }},
		{"invalid declared media type", func(work *EmbeddingWork) { work.SourceMediaType = "not a media type" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			work := base
			test.mutate(&work)
			blobs := &embeddingRuntimeTestBlobs{data: data}
			runtime, err := NewProviderEmbeddingRuntime(client, blobs, t.TempDir(),
				func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderTransient, time.Second })
			require.NoError(t, err)

			_, err = runtime.Prepare(t.Context(), work)
			require.Error(t, err)
			assert.Zero(t, blobs.opens)
			assert.Zero(t, secrets.Load())
		})
	}

	t.Run("disclosure transplant", func(t *testing.T) {
		record, binding := geminiPolicyProfile(t, descriptor, workerHash("other-disclosure"), 4096)
		work := fixtureGeminiPolicyWork(data, "synthetic.png", "image/png", record, binding, descriptor)
		blobs := &embeddingRuntimeTestBlobs{data: data}
		runtime, err := NewProviderEmbeddingRuntime(client, blobs, t.TempDir(),
			func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderTransient, time.Second })
		require.NoError(t, err)
		_, err = runtime.Prepare(t.Context(), work)
		require.ErrorContains(t, err, "disclosure")
		assert.Zero(t, blobs.opens)
		assert.Zero(t, secrets.Load())
	})
}

func newGeminiPolicyClient(t *testing.T, maxInputBytes int64, capability string) (*geminiembed.Client,
	document.EmbeddingDescriptor, string, *atomic.Int32,
) {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "gemini-embedding-2/search/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "title: none | text: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "task: search result | query: {{content}}"},
	})
	require.NoError(t, err)
	disclosure := workerHash("gemini-disclosure")
	profile := geminiembed.Profile{
		CompatibilityEpoch: "epoch-1", SecretBinding: "secret:gemini", Transport: geminiembed.TransportInline,
		CapabilityProfileFingerprint: capability, DisclosureFingerprint: disclosure,
		RequestTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaxPollAttempts: 3,
		CleanupTimeout: time.Second, MaxInputBytes: maxInputBytes, MaxRequestBytes: 2 << 20, MaxResponseBytes: 1 << 20,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "generativelanguage.googleapis.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, ProxyMode: providerhttp.ProxyDisabled,
			ConnectTimeout: time.Second, KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
		},
		Descriptor: document.EmbeddingDescriptor{
			ID: geminiembed.ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustHostedProvider, Model: geminiembed.Model, ModelRevision: "epoch-1",
			Dimension: 128, Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationUnitLength,
			ScalarEncoding: geminiembed.ScalarEncodingFloat32, DocumentFormatter: geminiembed.DocumentFormatterV1,
			QueryFormatter:  geminiembed.QueryFormatterV1,
			InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
	}
	policyFingerprint, err := geminiembed.PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = policyFingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	secretCalls := new(atomic.Int32)
	client, err := geminiembed.New(profile, geminiPolicySecrets{calls: secretCalls}, geminiPolicyResolver{}, &http.Client{})
	require.NoError(t, err)
	return client, profile.Descriptor, disclosure, secretCalls
}

func geminiPolicyProfile(t *testing.T, descriptor document.EmbeddingDescriptor, disclosure string,
	maxInputBytes int64,
) (store.ProcessingProfileRecord, document.EmbeddingBindingV1) {
	t.Helper()
	binding := document.EmbeddingBindingV1{
		Activation: document.EmbeddingOptional, AuthorizationFingerprint: workerHash("gemini-authorization"),
		CompatibilityID: descriptor.CompatibilityID, CredentialBinding: "credential:gemini",
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, DisclosureFingerprint: disclosure,
		DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputOriginalFile,
		MaxBatchItems: 1, MaxInputBytes: maxInputBytes, MaxResponseBytes: 1 << 20,
		Metric: descriptor.Metric, ModelInput: descriptor.ModelInput, Model: descriptor.Model, Name: "gemini",
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1, Embeddings: []document.EmbeddingBindingV1{binding},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			MaxDocumentChars: 10_000, CompletenessFingerprint: workerHash("complete"),
			LexicalSegmenterFingerprint: workerHash("lexical"), MaxSegmentRunes: 1_000, MaxUnitRunes: 1_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, NormalizerFingerprint: workerHash("normalizer"),
			RenditionContract: document.RenditionContractV1, SanitizerFingerprint: workerHash("sanitizer"),
			SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: workerHash("attachment"), ConsentFingerprint: workerHash("consent"),
			TrustBoundary: string(document.EmbeddingTrustHostedProvider),
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 10, VectorLimit: 10},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	record := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	require.NoError(t, store.ValidateProcessingProfileRecord(record))
	return record, binding
}

func fixtureGeminiPolicyWork(data []byte, filename, mediaType string, record store.ProcessingProfileRecord,
	binding document.EmbeddingBindingV1, descriptor document.EmbeddingDescriptor,
) EmbeddingWork {
	return EmbeddingWork{ContentVersionID: "00000000-0000-4000-8000-000000000001",
		ProcessingProfile: record, Binding: binding, Descriptor: descriptor,
		SourceBlobHash: workerHashBytes(data), SourceBytes: int64(len(data)),
		SourceFilename: filename, SourceMediaType: mediaType}
}

type geminiPolicySecrets struct{ calls *atomic.Int32 }

func (secrets geminiPolicySecrets) ResolveSecret(context.Context, string) (string, error) {
	secrets.calls.Add(1)
	return "synthetic-key", nil
}

type geminiPolicyResolver struct{}

func (geminiPolicyResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
}

func TestProviderEmbeddingRuntimeClassifiesUnconfirmedRetentionBeforeCallback(t *testing.T) {
	fixture := newEmbeddingWorkerFixture(t)
	provider := &embeddingWorkerProvider{runtime: fixture.runtime, binding: "direct", descriptor: fixture.descriptor}
	var classifierCalls atomic.Int32
	runtime, err := NewProviderEmbeddingRuntime(provider, &embeddingRuntimeTestBlobs{}, t.TempDir(),
		func(error) (EmbeddingProviderFailure, time.Duration) {
			classifierCalls.Add(1)
			return EmbeddingProviderTransient, time.Second
		})
	require.NoError(t, err)
	joined := errors.Join(embeddingTransientError{retryAfter: time.Second},
		contextualRetentionError{cause: geminiembed.ErrRemoteRetentionUnconfirmed})

	failure, delay := runtime.Classify(joined)

	assert.Equal(t, EmbeddingProviderPermanent, failure)
	assert.Zero(t, delay)
	assert.Zero(t, classifierCalls.Load())

	failure, delay = runtime.Classify(embeddingTransientError{retryAfter: 3 * time.Millisecond})
	assert.Equal(t, EmbeddingProviderTransient, failure)
	assert.Equal(t, time.Second, delay)
	assert.Equal(t, int32(1), classifierCalls.Load())

	var nilRuntime *ProviderEmbeddingRuntime
	failure, delay = nilRuntime.Classify(joined)
	assert.Equal(t, EmbeddingProviderPermanent, failure)
	assert.Zero(t, delay)
	failure, delay = nilRuntime.Classify(errors.New("ordinary"))
	assert.Equal(t, EmbeddingProviderPermanent, failure)
	assert.Zero(t, delay)
}

func TestProviderEmbeddingRuntimePreparedPathsUseGuardedClassification(t *testing.T) {
	joined := errors.Join(embeddingTransientError{}, geminiembed.ErrRemoteRetentionUnconfirmed)

	t.Run("original direct", func(t *testing.T) {
		fixture := newEmbeddingWorkerFixture(t)
		work := fixture.work("guarded-direct", document.EmbeddingInputOriginalFile, "direct")
		data := mediatest.PNG(2, 2, color.White)
		work.SourceBlobHash = workerHashBytes(data)
		work.SourceBytes = int64(len(data))
		work.SourceFilename = "synthetic.png"
		work.SourceMediaType = "image/png"
		provider := &embeddingWorkerProvider{runtime: fixture.runtime, binding: work.Binding.Name, descriptor: work.Descriptor}
		runtime, err := NewProviderEmbeddingRuntime(provider, &embeddingRuntimeTestBlobs{data: data}, t.TempDir(),
			func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderTransient, time.Second })
		require.NoError(t, err)
		execution, err := runtime.Prepare(t.Context(), work)
		require.NoError(t, err)
		t.Cleanup(func() { closeEmbeddingInputs(execution.Inputs) })

		failure, delay := execution.Classify(joined)
		assert.Equal(t, EmbeddingProviderPermanent, failure)
		assert.Zero(t, delay)

		registry := NewEmbeddingRuntimeRegistry()
		require.NoError(t, registry.Register(work.Descriptor.Fingerprint, runtime))
		execution, err = registry.Prepare(t.Context(), work)
		require.NoError(t, err)
		t.Cleanup(func() { closeEmbeddingInputs(execution.Inputs) })
		failure, delay = execution.Classify(joined)
		assert.Equal(t, EmbeddingProviderPermanent, failure)
		assert.Zero(t, delay)
	})

	t.Run("rendition chunk", func(t *testing.T) {
		fixture, _, worker, _ := newRealEmbeddingWorker(t, document.EmbeddingInputRenditionChunk)
		claim, work, found, err := fixture.catalog.ClaimNextEmbeddingWork(t.Context(), "classifier-test",
			time.Now().UTC(), 3*time.Second, worker.descriptorFingerprints)
		require.NoError(t, err)
		require.True(t, found)
		t.Cleanup(func() { _ = fixture.catalog.AbandonEmbeddingWork(t.Context(), claim, time.Now().UTC()) })

		execution, err := worker.runtime.Prepare(t.Context(), work)
		require.NoError(t, err)
		failure, delay := execution.Classify(joined)
		assert.Equal(t, EmbeddingProviderPermanent, failure)
		assert.Zero(t, delay)
	})
}

type contextualRetentionError struct{ cause error }

func (failure contextualRetentionError) Error() string { return "synthetic retention context" }
func (failure contextualRetentionError) Unwrap() error { return failure.cause }
