package mistral

import (
	"context"
	json "encoding/json/v2"
	"errors"
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
)

type embeddingSecretMap map[string]string

func (secrets embeddingSecretMap) ResolveSecret(_ context.Context, name string) (string, error) {
	return secrets[name], nil
}

func TestEmbeddingProviderAppliesDocumentEnvelopeAndRestoresIndices(t *testing.T) {
	var request struct {
		Input          []string `json:"input"`
		Model          string   `json:"model"`
		EncodingFormat string   `json:"encoding_format"`
	}
	endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		assert.Equal(t, "/v1/embeddings", incoming.URL.Path)
		assert.Equal(t, "Bearer synthetic-secret", incoming.Header.Get("Authorization"))
		assert.NoError(t, json.UnmarshalRead(incoming.Body, &request, json.RejectUnknownMembers(true)))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(mistralEmbeddingBody(t, []int{1, 0}))
	}))
	profile := testEmbeddingProfile(t)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	recomputeMistralEmbeddingProfile(t, &profile)
	provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "synthetic-secret"}, resolver)
	require.NoError(t, err)
	inputs := []document.EmbeddingInput{
		{Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"},
		{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "second"},
	}
	result, err := document.ExecuteEmbedding(t.Context(), provider, inputs, embeddingAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.Equal(t, []string{"document envelope: passage", "document envelope: second"}, request.Input)
	assert.Equal(t, []string{"first", "second"}, []string{result.Vectors[0].Key, result.Vectors[1].Key})
}

func TestMistralEmbeddingPolicyFingerprintCoversCompleteContractAndEgress(t *testing.T) {
	profile := testEmbeddingProfile(t)
	baseline, err := EmbeddingPolicyFingerprint(profile)
	require.NoError(t, err)
	changed := profile
	changed.ModelInput = testMistralModelInput(t, "changed: {{content}}", "query envelope: {{content}}")
	changed.Descriptor.ModelInput, changed.Descriptor.CompatibilityID = changed.ModelInput, changed.ModelInput.CompatibilityID
	recomputeMistralEmbeddingProfile(t, &changed)
	assert.NotEqual(t, baseline, changed.Descriptor.PolicyFingerprint)
	changed = profile
	changed.EgressPolicy.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	recomputeMistralEmbeddingProfile(t, &changed)
	assert.NotEqual(t, baseline, changed.Descriptor.PolicyFingerprint)
}

func TestMistralEmbeddingRejectsMalformedAndPartialResponsesPrivately(t *testing.T) {
	one, two := mistralUnitEmbedding(0), mistralUnitEmbedding(1)
	valid := func(indices []int, vectors [][]float32) []byte {
		items := make([]map[string]any, len(indices))
		for index := range indices {
			items[index] = map[string]any{"object": "embedding", "embedding": vectors[index], "index": indices[index]}
		}
		body, err := json.Marshal(map[string]any{"id": "synthetic", "object": "list", "data": items, "model": EmbeddingModel, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 0, "total_tokens": 2, "prompt_audio_seconds": nil}})
		require.NoError(t, err)
		return body
	}
	tests := []struct {
		name string
		body []byte
	}{
		{"malformed", []byte(`{"object":`)}, {"unknown", []byte(`{"unknown":"PRIVATE_RAW_BODY"}`)}, {"duplicate", []byte(`{"model":"mistral-embed","model":"PRIVATE_RAW_BODY"}`)},
		{"partial indices", valid([]int{0}, [][]float32{one})}, {"duplicate index", valid([]int{0, 0}, [][]float32{one, two})}, {"out of range", valid([]int{0, 2}, [][]float32{one, two})}, {"wrong dimension", valid([]int{0, 1}, [][]float32{one[:1023], two})},
		{"zero vector", valid([]int{0, 1}, [][]float32{make([]float32, 1024), two})}, {"non finite", []byte(`{"id":"x","object":"list","data":[{"object":"embedding","embedding":[1e999],"index":0}],"model":"mistral-embed","usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1,"prompt_audio_seconds":null}}`)},
	}
	inputs := []document.EmbeddingInput{{Key: "a", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "PRIVATE_INPUT"}, {Key: "b", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "other"}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(test.body)
			}))
			profile := testEmbeddingProfile(t)
			profile.Endpoint, profile.EgressPolicy = endpoint, egress
			recomputeMistralEmbeddingProfile(t, &profile)
			provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "PRIVATE_SECRET"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), inputs, embeddingAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "PRIVATE")
		})
	}
}

func TestMistralEmbeddingClassifiesCapacityPermanentAndTransient(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}
	for _, test := range []struct {
		name   string
		status int
		kind   error
	}{{"capacity", http.StatusRequestEntityTooLarge, ErrEmbeddingCapacity}, {"permanent", http.StatusBadRequest, ErrPermanentResponse}, {"rate limit", http.StatusTooManyRequests, ErrTransientResponse}, {"transient", http.StatusServiceUnavailable, ErrTransientResponse}} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte("PRIVATE_BODY"))
			}))
			profile := testEmbeddingProfile(t)
			profile.Endpoint, profile.EgressPolicy, profile.MaxRetries, profile.MaxRetryDelay = endpoint, egress, 2, time.Millisecond
			recomputeMistralEmbeddingProfile(t, &profile)
			provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
			require.NoError(t, err)
			_, err = provider.Embed(t.Context(), input, embeddingAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, test.kind)
			assert.NotContains(t, err.Error(), "PRIVATE_BODY")
			metrics := MetricsFromError(err)
			assert.Equal(t, int(calls.Load()), metrics.Requests)
			if errors.Is(test.kind, ErrTransientResponse) {
				assert.Equal(t, 2, metrics.Requests)
				assert.Equal(t, 1, metrics.Retries)
			}
		})
	}
}

func TestMistralEmbeddingRejectsIdentityDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*EmbeddingProfile)
	}{
		{"model", func(profile *EmbeddingProfile) { profile.Descriptor.Model = "mistral-embed-drift" }},
		{"revision", func(profile *EmbeddingProfile) { profile.Descriptor.ModelRevision = "invented-revision" }},
		{"compatibility", func(profile *EmbeddingProfile) { profile.Descriptor.CompatibilityID = "mistral/other-space/v1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := testEmbeddingProfile(t)
			test.mutate(&profile)
			_, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, nil)
			require.Error(t, err)
		})
	}
}

func TestMistralEmbeddingResponseLimitAndTransportExhaustion(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}
	t.Run("response limit", func(t *testing.T) {
		endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(strings.Repeat("x", 8193)))
		}))
		profile := testEmbeddingProfile(t)
		profile.Endpoint, profile.EgressPolicy, profile.MaxResponseBytes = endpoint, egress, 8192
		recomputeMistralEmbeddingProfile(t, &profile)
		provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
		require.NoError(t, err)
		authorization := embeddingAuthorization(profile.Descriptor)
		authorization.MaxResponseBytes = 8192
		_, err = provider.Embed(t.Context(), input, authorization)
		require.ErrorIs(t, err, ErrPermanentResponse)
	})
	t.Run("transport exhaustion", func(t *testing.T) {
		profile := testEmbeddingProfile(t)
		profile.MaxRetries, profile.MaxRetryDelay = 2, time.Millisecond
		recomputeMistralEmbeddingProfile(t, &profile)
		provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, failingMistralEmbeddingResolver{})
		require.NoError(t, err)
		_, err = provider.Embed(t.Context(), input, embeddingAuthorization(profile.Descriptor))
		require.ErrorIs(t, err, ErrTransientResponse)
		assert.Equal(t, 2, MetricsFromError(err).Requests)
	})
}

type failingMistralEmbeddingResolver struct{}

func (failingMistralEmbeddingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("synthetic DNS failure")
}

func TestMistralEmbeddingRequestBoundRedirectAndCancellation(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}
	endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "http://elsewhere.invalid/private")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	profile := testEmbeddingProfile(t)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	recomputeMistralEmbeddingProfile(t, &profile)
	provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
	require.NoError(t, err)
	_, err = provider.Embed(t.Context(), input, embeddingAuthorization(profile.Descriptor))
	require.ErrorIs(t, err, ErrPermanentResponse)
	bounded := profile
	bounded.MaxRequestBytes = 1
	recomputeMistralEmbeddingProfile(t, &bounded)
	provider, err = NewEmbeddingProvider(bounded, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
	require.NoError(t, err)
	_, err = provider.Embed(t.Context(), input, embeddingAuthorization(bounded.Descriptor))
	require.ErrorIs(t, err, ErrEmbeddingCapacity)
}

func TestMistralEmbeddingAcceptsDocumentedOptionalUsageFields(t *testing.T) {
	endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		body := mistralEmbeddingBody(t, []int{0})
		body = []byte(strings.Replace(string(body), `"prompt_tokens":2`, `"prompt_tokens":2,"prompt_tokens_details":{"cached_tokens":1},"prompt_token_details":{"cached_tokens":1},"num_cached_tokens":1,"service_tier":"standard"`, 1))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(body)
	}))
	profile := testEmbeddingProfile(t)
	profile.Endpoint, profile.EgressPolicy = endpoint, egress
	recomputeMistralEmbeddingProfile(t, &profile)
	provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
	require.NoError(t, err)
	_, err = provider.Embed(t.Context(), []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}, embeddingAuthorization(profile.Descriptor))
	require.NoError(t, err)
}

func TestMistralEmbeddingCancellationPreservesIdentityAndMetrics(t *testing.T) {
	input := []document.EmbeddingInput{{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "text"}}
	t.Run("retry wait", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Retry-After", "60")
			writer.WriteHeader(http.StatusServiceUnavailable)
		}))
		profile := testEmbeddingProfile(t)
		profile.Endpoint, profile.EgressPolicy = endpoint, egress
		recomputeMistralEmbeddingProfile(t, &profile)
		provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
		require.NoError(t, err)
		_, err = provider.Embed(ctx, input, embeddingAuthorization(profile.Descriptor))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, RequestMetrics{Requests: 1, Retries: 1}, withoutLatency(MetricsFromError(err)))
	})

	t.Run("response read", func(t *testing.T) {
		started := make(chan struct{})
		endpoint, egress, resolver := mistralFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			flusher, ok := writer.(http.Flusher)
			if !assert.True(t, ok) {
				return
			}
			flusher.Flush()
			close(started)
			<-request.Context().Done()
		}))
		profile := testEmbeddingProfile(t)
		profile.Endpoint, profile.EgressPolicy = endpoint, egress
		recomputeMistralEmbeddingProfile(t, &profile)
		provider, err := NewEmbeddingProvider(profile, embeddingSecretMap{"credential:mistral-embed": "secret"}, resolver)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, embedErr := provider.Embed(ctx, input, embeddingAuthorization(profile.Descriptor))
			done <- embedErr
		}()
		<-started
		cancel()
		err = <-done
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, RequestMetrics{Requests: 1}, withoutLatency(MetricsFromError(err)))
	})
}

func withoutLatency(metrics RequestMetrics) RequestMetrics { metrics.Latency = 0; return metrics }

func recomputeMistralEmbeddingProfile(t *testing.T, profile *EmbeddingProfile) {
	t.Helper()
	profile.Descriptor.PolicyFingerprint = strings.Repeat("0", 64)
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, _ = document.NewEmbeddingDescriptor(profile.Descriptor)
	fingerprint, err := EmbeddingPolicyFingerprint(*profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
}

func mistralUnitEmbedding(hot int) []float32 {
	vector := make([]float32, 1024)
	vector[hot] = 1
	return vector
}

func testEmbeddingProfile(t *testing.T) EmbeddingProfile {
	t.Helper()
	modelInput := testMistralModelInput(t, "document envelope: {{content}}", "query envelope: {{content}}")
	profile := EmbeddingProfile{Endpoint: embeddingOrigin, EgressPolicy: productionMistralEgress(), ModelInput: modelInput, SecretBinding: "credential:mistral-embed", MaxBatchItems: 8, MaxInputBytes: 4096, MaxRequestBytes: 8192, MaxResponseBytes: 1 << 20, Descriptor: document.EmbeddingDescriptor{ID: EmbeddingProviderID, ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: strings.Repeat("0", 64), TrustBoundary: document.EmbeddingTrustHostedProvider, Model: EmbeddingModel, ModelRevision: EmbeddingHostedAliasRevision, Dimension: 1024, Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: EmbeddingScalarFloat32, DocumentFormatter: EmbeddingDocumentFormatterV1, QueryFormatter: EmbeddingQueryFormatterV1, InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: modelInput.CompatibilityID, ModelInput: modelInput, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}}}
	recomputeMistralEmbeddingProfile(t, &profile)
	return profile
}

func productionMistralEgress() providerhttp.EgressPolicy {
	return providerhttp.EgressPolicy{Scheme: "https", Host: "api.mistral.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}, ProxyMode: providerhttp.ProxyDisabled}
}

func testMistralModelInput(t *testing.T, documentTemplate, queryTemplate string) document.ModelInputContract {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "mistral/test-space/v1", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: documentTemplate}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: queryTemplate}})
	require.NoError(t, err)
	return contract
}

func embeddingAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 8, MaxInputBytes: 4096, MaxResponseBytes: 1 << 20}
}

func mistralEmbeddingBody(t *testing.T, indices []int) []byte {
	t.Helper()
	items := make([]map[string]any, len(indices))
	for position, index := range indices {
		items[position] = map[string]any{"object": "embedding", "embedding": mistralUnitEmbedding(index + 1), "index": index}
	}
	body, err := json.Marshal(map[string]any{"id": "synthetic-response", "object": "list", "data": items, "model": EmbeddingModel, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 0, "total_tokens": 2, "prompt_audio_seconds": nil}})
	require.NoError(t, err)
	return body
}
