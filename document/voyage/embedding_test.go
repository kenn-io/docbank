package voyage_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
)

type embeddingSecrets map[string]string

func (secrets embeddingSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	return secrets[name], nil
}

type textEmbeddingRequest struct {
	Input           []string   `json:"input"`
	Inputs          [][]string `json:"inputs"`
	Model           string     `json:"model"`
	InputType       string     `json:"input_type"`
	Truncation      *bool      `json:"truncation"`
	OutputDimension int        `json:"output_dimension"`
	OutputDType     string     `json:"output_dtype"`
}

func TestEmbeddingProviderSendsVoyageDocumentRoleAndEnvelope(t *testing.T) {
	var decoded textEmbeddingRequest
	endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/v1/embeddings", request.URL.Path)
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		assert.NoError(t, json.UnmarshalRead(request.Body, &decoded, json.RejectUnknownMembers(true)))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(voyageTextBody(t, voyage.TextModel, []int{0}, []int{1}))
	}))
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile = refingerprintVoyageProfile(t, profile)
	provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.NoError(t, err)
	inputs := []document.EmbeddingInput{{Key: "doc", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}
	result, err := document.ExecuteEmbedding(t.Context(), provider, inputs, voyageAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.Equal(t, "document", decoded.InputType)
	assert.Equal(t, []string{"document envelope: passage"}, decoded.Input)
	require.NotNil(t, decoded.Truncation)
	assert.False(t, *decoded.Truncation)
	assert.Equal(t, "doc", result.Vectors[0].Key)
}

func TestVoyageEmbeddingPolicyFingerprintCoversDisclosureAndRoleContract(t *testing.T) {
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	baseline, err := voyage.EmbeddingPolicyFingerprint(profile)
	require.NoError(t, err)

	changedEnvelope := profile
	changedEnvelope.ModelInput = customVoyageInput(t, "changed: {{content}}", "query envelope: {{content}}")
	changedEnvelope.Descriptor.ModelInput = changedEnvelope.ModelInput
	changedEnvelope.Descriptor.CompatibilityID = changedEnvelope.ModelInput.CompatibilityID
	changedEnvelope = refingerprintVoyageProfile(t, changedEnvelope)
	assert.NotEqual(t, baseline, changedEnvelope.Descriptor.PolicyFingerprint)

	changedEgress := profile
	changedEgress.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	changedEgress = refingerprintVoyageProfile(t, changedEgress)
	assert.NotEqual(t, baseline, changedEgress.Descriptor.PolicyFingerprint)
}

func TestEmbeddingProviderRejectsMalformedIndexedResponsesPrivately(t *testing.T) {
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	one, two := unitEmbedding(0), unitEmbedding(1)
	valid := func(indices []int, vectors [][]float32) []byte {
		items := make([]map[string]any, len(indices))
		for index := range indices {
			items[index] = map[string]any{"object": "embedding", "embedding": vectors[index], "index": indices[index]}
		}
		body, err := json.Marshal(map[string]any{"object": "list", "data": items, "model": profile.Descriptor.Model, "usage": map[string]any{"total_tokens": 1}})
		require.NoError(t, err)
		return body
	}
	tests := []struct {
		name string
		body []byte
	}{
		{"malformed", []byte(`{"object":`)},
		{"unknown", []byte(`{"object":"list","unknown":"PRIVATE_RAW_BODY","data":[],"model":"voyage-4"}`)},
		{"duplicate key", []byte(`{"object":"list","model":"voyage-4","model":"PRIVATE_RAW_BODY","data":[]}`)},
		{"partial indices", valid([]int{0}, [][]float32{one})},
		{"duplicate index", valid([]int{0, 0}, [][]float32{one, two})},
		{"out of range", valid([]int{0, 2}, [][]float32{one, two})},
		{"wrong dimension", valid([]int{0, 1}, [][]float32{one[:255], two})},
		{"zero vector", valid([]int{0, 1}, [][]float32{make([]float32, 256), two})},
		{"non finite", []byte(`{"object":"list","data":[{"object":"embedding","embedding":[1e999],"index":0}],"model":"voyage-4"}`)},
	}
	inputs := []document.EmbeddingInput{
		{Key: "a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "PRIVATE_INPUT"},
		{Key: "b", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(test.body)
			}))
			local := profile
			local.Endpoint, local.EgressPolicy = endpoint, egress
			local = refingerprintVoyageProfile(t, local)
			provider, err := voyage.NewEmbeddingProvider(local, embeddingSecrets{"credential:voyage": "PRIVATE_SECRET"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), inputs, voyageAuthorization(local.Descriptor))
			require.ErrorIs(t, err, voyage.ErrMalformedResponse)
			assert.NotContains(t, err.Error(), "PRIVATE")
		})
	}
}

func TestEmbeddingProviderClassifiesCapacityAndExhaustedTransient(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}
	for _, test := range []struct {
		name   string
		status int
		kind   error
	}{
		{"auth", http.StatusUnauthorized, voyage.ErrUnauthorized},
		{"capacity", http.StatusRequestEntityTooLarge, voyage.ErrBatchTooLarge},
		{"permanent", http.StatusBadRequest, voyage.ErrPermanentResponse},
		{"rate limit", http.StatusTooManyRequests, voyage.ErrTransientResponse},
		{"transient", http.StatusServiceUnavailable, voyage.ErrTransientResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte("PRIVATE_BODY"))
			}))
			profile := voyageTextProfile(t, voyage.EmbeddingModeText)
			profile.Endpoint, profile.EgressPolicy, profile.MaxRetries, profile.RetryBaseDelay = endpoint, egress, 2, time.Millisecond
			profile = refingerprintVoyageProfile(t, profile)
			provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), input, voyageAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, test.kind)
			assert.NotContains(t, err.Error(), "PRIVATE_BODY")
			metrics := voyage.MetricsFromError(err)
			assert.Equal(t, int(calls.Load()), metrics.Requests)
			if errors.Is(test.kind, voyage.ErrTransientResponse) {
				assert.Equal(t, 2, metrics.Requests)
				assert.Equal(t, 1, metrics.Retries)
			}
		})
	}
}

func TestVoyageEmbeddingRejectsIdentityDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*voyage.EmbeddingProfile)
	}{
		{"model", func(profile *voyage.EmbeddingProfile) { profile.Descriptor.Model = "voyage-4-drift" }},
		{"revision", func(profile *voyage.EmbeddingProfile) { profile.Descriptor.ModelRevision = "invented-revision" }},
		{"compatibility", func(profile *voyage.EmbeddingProfile) { profile.Descriptor.CompatibilityID = "voyage/other-space/v1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := voyageTextProfile(t, voyage.EmbeddingModeText)
			test.mutate(&profile)
			_, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, nil)
			require.Error(t, err)
		})
	}
}

func TestVoyageEmbeddingResponseLimitCancellationAndTransportFailure(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "PRIVATE_INPUT"}}
	t.Run("response limit", func(t *testing.T) {
		endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(strings.Repeat("x", 4097)))
		}))
		profile := voyageTextProfile(t, voyage.EmbeddingModeText)
		profile.Endpoint, profile.EgressPolicy, profile.MaxResponseBytes = endpoint, egress, 4096
		profile = refingerprintVoyageProfile(t, profile)
		provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, resolver)
		require.NoError(t, err)
		authorization := voyageAuthorization(profile.Descriptor)
		authorization.MaxResponseBytes = 4096
		_, err = provider.Embed(t.Context(), input, authorization)
		require.ErrorIs(t, err, voyage.ErrMalformedResponse)
	})
	t.Run("cancellation", func(t *testing.T) {
		endpoint, egress, resolver := voyageFixture(t, http.NotFoundHandler())
		profile := voyageTextProfile(t, voyage.EmbeddingModeText)
		profile.Endpoint, profile.EgressPolicy = endpoint, egress
		profile = refingerprintVoyageProfile(t, profile)
		provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, resolver)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = provider.Embed(ctx, input, voyageAuthorization(profile.Descriptor))
		require.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), "PRIVATE_INPUT")
	})
	t.Run("transport exhaustion", func(t *testing.T) {
		profile := voyageTextProfile(t, voyage.EmbeddingModeText)
		profile.MaxRetries, profile.RetryBaseDelay = 2, time.Millisecond
		profile = refingerprintVoyageProfile(t, profile)
		provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, failingEmbeddingResolver{})
		require.NoError(t, err)
		_, err = provider.Embed(t.Context(), input, voyageAuthorization(profile.Descriptor))
		require.ErrorIs(t, err, voyage.ErrTransientResponse)
		assert.Equal(t, 2, voyage.MetricsFromError(err).Requests)
	})
}

type failingEmbeddingResolver struct{}

func (failingEmbeddingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("synthetic DNS failure")
}

func TestEmbeddingProviderBoundsRequestBytesAndRefusesRedirect(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}
	endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "http://elsewhere.invalid/private")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	profile := voyageTextProfile(t, voyage.EmbeddingModeText)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	profile = refingerprintVoyageProfile(t, profile)
	provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, resolver)
	require.NoError(t, err)
	_, err = provider.Embed(t.Context(), input, voyageAuthorization(profile.Descriptor))
	require.ErrorIs(t, err, voyage.ErrPermanentResponse)

	bounded := profile
	bounded.MaxRequestBytes = 1
	bounded = refingerprintVoyageProfile(t, bounded)
	provider, err = voyage.NewEmbeddingProvider(bounded, embeddingSecrets{"credential:voyage": "secret"}, resolver)
	require.NoError(t, err)
	_, err = provider.Embed(t.Context(), input, voyageAuthorization(bounded.Descriptor))
	require.ErrorIs(t, err, voyage.ErrBatchTooLarge)
}

func TestDirectFileProfileRemainsCapabilityAttestedExportOnly(t *testing.T) {
	policy := testPolicy(t)
	manifest, err := voyagetest.SyntheticManifest(policy)
	require.NoError(t, err)
	profile := voyageDirectFileProfile(t, policy, manifest)
	provider, err := voyage.NewEmbeddingProvider(profile, embeddingSecrets{"credential:voyage": "secret"}, embeddingResolver{address: netip.MustParseAddr("192.0.2.1")})
	require.NoError(t, err)
	assert.False(t, provider.Descriptor().SupportsTextQuery)
}

func unitEmbedding(hot int) []float32 {
	vector := make([]float32, 256)
	vector[hot] = 1
	return vector
}

func voyageDirectFileProfile(t *testing.T, policy voyage.Policy, manifest voyage.CapabilityManifest) voyage.EmbeddingProfile {
	t.Helper()
	modelInput := customVoyageInput(t, "document envelope: {{content}}", "query envelope: {{content}}")
	revision, err := voyage.DirectFileModelRevision(policy, manifest)
	require.NoError(t, err)
	profile := voyage.EmbeddingProfile{
		Mode: voyage.EmbeddingModeDirectFile, Endpoint: voyage.DefaultEndpoint, EgressPolicy: productionVoyageEgress(), ModelInput: modelInput,
		SecretBinding: "credential:voyage", MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxRequestBytes: voyage.MaxRequestBytes,
		MaxResponseBytes: 1 << 20, Policy: policy, CapabilityManifest: manifest,
		Descriptor: document.EmbeddingDescriptor{
			ID: voyage.EmbeddingProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			PolicyFingerprint: strings.Repeat("0", 64), TrustBoundary: document.EmbeddingTrustHostedProvider,
			Model: voyage.DefaultModel, ModelRevision: revision, Dimension: voyage.DefaultDimension,
			Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationUnitLength,
			ScalarEncoding: voyage.EmbeddingScalarFloat32, DocumentFormatter: voyage.EmbeddingDocumentFormatterV1,
			QueryFormatter: voyage.EmbeddingQueryFormatterV1, InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
			CompatibilityID: modelInput.CompatibilityID, ModelInput: modelInput,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeDocument},
		},
	}
	return refingerprintVoyageProfile(t, profile)
}

func voyageTextProfile(t *testing.T, mode voyage.EmbeddingMode) voyage.EmbeddingProfile {
	t.Helper()
	model := voyage.TextModel
	if mode == voyage.EmbeddingModeContextual {
		model = voyage.ContextualModel
	}
	modelInput := customVoyageInput(t, "document envelope: {{content}}", "query envelope: {{content}}")
	profile := voyage.EmbeddingProfile{
		Mode: mode, Endpoint: voyage.DefaultEndpoint, EgressPolicy: productionVoyageEgress(), ModelInput: modelInput,
		SecretBinding: "credential:voyage", ChunkerVersion: "1.0.0", MaxBatchItems: 8, MaxInputBytes: 4096,
		MaxRequestBytes: 8192, MaxResponseBytes: 1 << 20,
		Descriptor: document.EmbeddingDescriptor{
			ID: voyage.EmbeddingProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			PolicyFingerprint: strings.Repeat("0", 64), TrustBoundary: document.EmbeddingTrustHostedProvider,
			Model: model, ModelRevision: voyage.HostedAliasRevision, Dimension: 256, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: voyage.EmbeddingScalarFloat32,
			DocumentFormatter: voyage.EmbeddingDocumentFormatterV1, QueryFormatter: voyage.EmbeddingQueryFormatterV1,
			InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: modelInput.CompatibilityID,
			ModelInput: modelInput, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery},
		},
	}
	return refingerprintVoyageProfile(t, profile)
}

func productionVoyageEgress() providerhttp.EgressPolicy {
	return providerhttp.EgressPolicy{Scheme: "https", Host: "api.voyageai.com", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}, ProxyMode: providerhttp.ProxyDisabled}
}

func customVoyageInput(t *testing.T, documentTemplate, queryTemplate string) document.ModelInputContract {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "voyage/test-space/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: documentTemplate},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: queryTemplate},
	})
	require.NoError(t, err)
	return contract
}

func voyageAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 8, MaxInputBytes: 4096, MaxResponseBytes: 1 << 20}
}

func voyageTextBody(t *testing.T, model string, indices, hot []int) []byte {
	t.Helper()
	items := make([]map[string]any, len(indices))
	for position, index := range indices {
		items[position] = map[string]any{"object": "embedding", "embedding": unitEmbedding(hot[position]), "index": index}
	}
	body, err := json.Marshal(map[string]any{"object": "list", "data": items, "model": model, "usage": map[string]any{"total_tokens": 2}})
	require.NoError(t, err)
	return body
}

var _ io.Reader
