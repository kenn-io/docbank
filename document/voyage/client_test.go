package voyage_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
)

// wire mirrors the provider request and response shapes for the fake server.
type wireRequest struct {
	Inputs []struct {
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			ImageBase64 string `json:"image_base64"`
			VideoBase64 string `json:"video_base64"`
		} `json:"content"`
	} `json:"inputs"`
	Model           string `json:"model"`
	InputType       string `json:"input_type"`
	Truncation      bool   `json:"truncation"`
	OutputDimension int    `json:"output_dimension"`
}

type wireItem struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

func unitVector(hot int, scale float32) []float32 {
	vector := make([]float32, voyage.DefaultDimension)
	vector[hot%voyage.DefaultDimension] = scale
	return vector
}

func writeVectors(t *testing.T, w http.ResponseWriter, vectors [][]float32, tokens int64) {
	t.Helper()
	items := make([]wireItem, len(vectors))
	for index, vector := range vectors {
		items[index] = wireItem{Embedding: vector, Index: index}
	}
	body := map[string]any{"data": items, "usage": map[string]any{"total_tokens": tokens}}
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

func decodeRequest(t *testing.T, r *http.Request) wireRequest {
	t.Helper()
	var request wireRequest
	assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
	return request
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func newServerClient(t *testing.T, server *httptest.Server, policy voyage.Policy, config voyage.ClientConfig) *voyage.Client {
	t.Helper()
	if config.APIKey == "" {
		config.APIKey = "synthetic-key"
	}
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := server.Client().Transport
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		return transport.RoundTrip(clone)
	})}
	client, err := voyage.NewClient(policy, config)
	require.NoError(t, err)
	return client
}

func mediaInput(t *testing.T, data []byte) *voyage.Media {
	t.Helper()
	metadata, err := media.DetectBytes(data, "")
	require.NoError(t, err)
	return &voyage.Media{Metadata: metadata, Bytes: data}
}

func fullAuthorizations(t *testing.T, policy voyage.Policy, passed ...string) []voyage.Authorization {
	t.Helper()
	manifest, err := voyagetest.SyntheticManifest(policy, passed...)
	require.NoError(t, err)
	authorizations, err := policy.AuthorizeAll(manifest)
	require.NoError(t, err)
	return authorizations
}

func TestNewClientValidatesOperationalBounds(t *testing.T) {
	policy := testPolicy(t)
	_, err := voyage.NewClient(policy, voyage.ClientConfig{})
	require.ErrorContains(t, err, "API key")
	_, err = voyage.NewClient(voyage.Policy{}, voyage.ClientConfig{APIKey: "k"})
	require.ErrorContains(t, err, "use NewPolicy")
	for name, config := range map[string]voyage.ClientConfig{
		"negative timeout":  {APIKey: "k", Timeout: -time.Second},
		"timeout above cap": {APIKey: "k", Timeout: voyage.MaxTimeout + time.Second},
		"negative retries":  {APIKey: "k", MaxRetries: -1},
		"retries above cap": {APIKey: "k", MaxRetries: voyage.MaxRetries + 1},
		"negative delay":    {APIKey: "k", RetryBaseDelay: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := voyage.NewClient(policy, config)
			require.ErrorContains(t, err, "operational bounds")
		})
	}
	client, err := voyage.NewClient(policy, voyage.ClientConfig{APIKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, policy.Values(), client.Policy().Values())
}

func TestEmbedDocumentsSendsOrderedInputsAndRestoresIndices(t *testing.T) {
	policy := testPolicy(t)
	var seen wireRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/multimodalembeddings", r.URL.Path)
		assert.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		seen = decodeRequest(t, r)
		// Return items out of order to prove index restoration.
		items := []wireItem{
			{Embedding: unitVector(1, 1), Index: 1},
			{Embedding: unitVector(0, 1), Index: 0},
			{Embedding: unitVector(2, 1), Index: 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items, "usage": map[string]any{"total_tokens": 42}})
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})

	jpeg := mediaInput(t, mediatest.JPEG(4, 4, color.White))
	png := mediaInput(t, mediatest.PNG(4, 4, color.White))
	mp4 := mediaInput(t, mediatest.MP4(64, 48, 900))
	result, err := client.EmbedDocuments(t.Context(), []voyage.Input{
		{Parts: []voyage.Part{{Text: "first context"}, {Media: jpeg}}},
		{Parts: []voyage.Part{{Media: png}}},
		{Parts: []voyage.Part{{Media: mp4}, {Text: "trailing context"}}},
	}, fullAuthorizations(t, policy))
	require.NoError(t, err)
	require.Len(t, result.Vectors, 3)
	for index, vector := range result.Vectors {
		assert.InDelta(t, float32(1), vector[index], 0, "vector %d must come from response index %d", index, index)
	}
	assert.Equal(t, voyage.Usage{TotalTokens: 42, Available: true}, result.Usage)
	assert.Equal(t, 1, result.Metrics.Requests)
	assert.Zero(t, result.Metrics.Retries)

	require.Len(t, seen.Inputs, 3)
	assert.Equal(t, voyage.DefaultModel, seen.Model)
	assert.Equal(t, "document", seen.InputType)
	assert.False(t, seen.Truncation)
	assert.Equal(t, voyage.DefaultDimension, seen.OutputDimension)
	assert.Equal(t, "text", seen.Inputs[0].Content[0].Type)
	assert.Equal(t, "first context", seen.Inputs[0].Content[0].Text)
	assert.Equal(t, "image_base64", seen.Inputs[0].Content[1].Type)
	assert.True(t, strings.HasPrefix(seen.Inputs[0].Content[1].ImageBase64, "data:image/jpeg;base64,"))
	assert.Equal(t, "video_base64", seen.Inputs[2].Content[0].Type)
	assert.True(t, strings.HasPrefix(seen.Inputs[2].Content[0].VideoBase64, "data:video/mp4;base64,"))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(seen.Inputs[1].Content[0].ImageBase64, "data:image/png;base64,"))
	require.NoError(t, err)
	assert.Equal(t, png.Bytes, decoded)
}

func TestEmbedDocumentsEnforcesCapabilityAuthorization(t *testing.T) {
	policy := testPolicy(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		request := decodeRequest(t, r)
		vectors := make([][]float32, len(request.Inputs))
		for index := range vectors {
			vectors[index] = unitVector(index, 1)
		}
		writeVectors(t, w, vectors, 1)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})
	png := mediaInput(t, mediatest.PNG(4, 4, color.White))
	gif := mediaInput(t, mediatest.GIF(4, 4, 3))
	pngOnly := fullAuthorizations(t, policy, voyage.CapabilityImagePNG)

	tests := []struct {
		name           string
		inputs         []voyage.Input
		authorizations []voyage.Authorization
		want           error
		wantText       string
	}{
		{name: "png alone passes", inputs: []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, authorizations: pngOnly},
		{name: "unauthorized format", inputs: []voyage.Input{{Parts: []voyage.Part{{Media: mediaInput(t, mediatest.JPEG(4, 4, nil))}}}}, authorizations: pngOnly, want: voyage.ErrCapabilityContract, wantText: "image_jpeg"},
		{name: "animated needs its own capability", inputs: []voyage.Input{{Parts: []voyage.Part{{Media: gif}}}}, authorizations: fullAuthorizations(t, policy, voyage.CapabilityImageGIFStill), want: voyage.ErrInvalidInput, wantText: "animated_not_allowed"},
		{name: "text needs interleaved", inputs: []voyage.Input{{Parts: []voyage.Part{{Text: "context"}, {Media: png}}}}, authorizations: pngOnly, want: voyage.ErrCapabilityContract, wantText: voyage.CapabilityInterleaved},
		{name: "batch needs batch capability", inputs: []voyage.Input{{Parts: []voyage.Part{{Media: png}}}, {Parts: []voyage.Part{{Media: png}}}}, authorizations: pngOnly, want: voyage.ErrCapabilityContract, wantText: voyage.CapabilityBatchLimits},
		{name: "no authorizations", inputs: []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, want: voyage.ErrCapabilityContract},
		{name: "no media part", inputs: []voyage.Input{{Parts: []voyage.Part{{Text: "only text"}}}}, authorizations: fullAuthorizations(t, policy), want: voyage.ErrInvalidInput, wantText: "exactly one media"},
		{name: "two media parts", inputs: []voyage.Input{{Parts: []voyage.Part{{Media: png}, {Media: png}}}}, authorizations: fullAuthorizations(t, policy), want: voyage.ErrInvalidInput},
		{name: "empty part", inputs: []voyage.Input{{Parts: []voyage.Part{{}, {Media: png}}}}, authorizations: fullAuthorizations(t, policy), want: voyage.ErrInvalidInput},
		{name: "text and media in one part", inputs: []voyage.Input{{Parts: []voyage.Part{{Text: "x", Media: png}}}}, authorizations: fullAuthorizations(t, policy), want: voyage.ErrInvalidInput},
		{name: "no parts", inputs: []voyage.Input{{}}, authorizations: fullAuthorizations(t, policy), want: voyage.ErrInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := calls.Load()
			_, err := client.EmbedDocuments(t.Context(), tt.inputs, tt.authorizations)
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)
			if tt.wantText != "" {
				require.ErrorContains(t, err, tt.wantText)
			}
			assert.Equal(t, before, calls.Load(), "rejected input must not reach the provider")
		})
	}
}

func TestEmbedDocumentsRejectsMismatchedMetadataAndForeignAuthorizations(t *testing.T) {
	policy := testPolicy(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeVectors(t, w, [][]float32{unitVector(0, 1)}, 1)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})
	authorizations := fullAuthorizations(t, policy)

	lying := mediaInput(t, mediatest.PNG(4, 4, color.White))
	lying.Metadata.Width = 9999
	_, err := client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: lying}}}}, authorizations)
	require.ErrorIs(t, err, voyage.ErrInvalidInput)
	require.ErrorContains(t, err, "does not describe its bytes")

	empty := &voyage.Media{Metadata: lying.Metadata}
	_, err = client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: empty}}}}, authorizations)
	require.ErrorIs(t, err, voyage.ErrInvalidInput)

	other, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.Policy{MaxBytes: 1 << 20, AllowStill: true}})
	require.NoError(t, err)
	foreign := fullAuthorizations(t, other)
	png := mediaInput(t, mediatest.PNG(4, 4, color.White))
	_, err = client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, foreign)
	require.ErrorIs(t, err, voyage.ErrCapabilityContract)
	require.ErrorContains(t, err, "not derived from this client's policy")

	result, err := client.EmbedDocuments(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, result.Vectors)
}

func TestEmbedQuerySupportsTextImageAndCombinedInputs(t *testing.T) {
	policy := testPolicy(t)
	var seen wireRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = decodeRequest(t, r)
		writeVectors(t, w, [][]float32{unitVector(3, 0.5)}, 7)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})
	png := mediaInput(t, mediatest.PNG(4, 4, color.White))
	all := fullAuthorizations(t, policy)
	textOnly := fullAuthorizations(t, policy, voyage.CapabilityQueryText)
	imageOnly := fullAuthorizations(t, policy, voyage.CapabilityQueryImage)

	vector, usage, err := client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Text: "red square"}}}, textOnly...)
	require.NoError(t, err)
	assert.InDelta(t, float32(0.5), vector[3], 0)
	assert.Equal(t, voyage.Usage{TotalTokens: 7, Available: true}, usage)
	assert.Equal(t, "query", seen.InputType)
	require.Len(t, seen.Inputs[0].Content, 1)
	assert.Equal(t, "text", seen.Inputs[0].Content[0].Type)

	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Media: png}}}, imageOnly...)
	require.NoError(t, err)
	assert.Equal(t, "image_base64", seen.Inputs[0].Content[0].Type)

	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Text: "red"}, {Media: png}}}, all...)
	require.NoError(t, err)
	require.Len(t, seen.Inputs[0].Content, 2)

	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Media: png}}}, textOnly...)
	require.ErrorIs(t, err, voyage.ErrCapabilityContract)
	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Text: "red"}}}, imageOnly...)
	require.ErrorIs(t, err, voyage.ErrCapabilityContract)
	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{}, all...)
	require.ErrorIs(t, err, voyage.ErrInvalidInput)
	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Media: mediaInput(t, mediatest.MP4(64, 48, 100))}}}, all...)
	require.ErrorIs(t, err, voyage.ErrInvalidInput)
	require.ErrorContains(t, err, "still image")
	_, _, err = client.EmbedQuery(t.Context(), voyage.Input{Parts: []voyage.Part{{Text: "a"}, {Text: "b"}}}, all...)
	require.ErrorIs(t, err, voyage.ErrInvalidInput)
}

func TestResponseValidationRejectsMalformedVectors(t *testing.T) {
	policy := testPolicy(t)
	tests := []struct {
		name string
		body string
	}{
		{name: "count", body: `{"data":[]}`},
		{name: "dimension", body: `{"data":[{"embedding":[1,2,3],"index":0}]}`},
		{name: "index out of range", body: `{"data":[{"embedding":` + vectorJSON() + `,"index":5}]}`},
		{name: "duplicate index", body: `{"data":[{"embedding":` + vectorJSON() + `,"index":0},{"embedding":` + vectorJSON() + `,"index":0}]}`},
		{name: "not json", body: `<html>`},
		{name: "nan", body: `{"data":[{"embedding":` + strings.Replace(vectorJSON(), "[0", "[NaN", 1) + `,"index":0}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 3, RetryBaseDelay: time.Millisecond})
			png := mediaInput(t, mediatest.PNG(2, 2, nil))
			if tt.name == "duplicate index" {
				_, err := client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: png}}}, {Parts: []voyage.Part{{Media: png}}}}, fullAuthorizations(t, policy))
				require.ErrorIs(t, err, voyage.ErrMalformedResponse)
			} else {
				_, err := client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, fullAuthorizations(t, policy))
				require.ErrorIs(t, err, voyage.ErrMalformedResponse)
				assert.False(t, voyage.IsRetryable(err))
				assert.Equal(t, 2, voyage.MetricsFromError(err).Requests, "malformed responses are retried once")
			}
			assert.Equal(t, int32(2), calls.Load())
		})
	}
}

func vectorJSON() string {
	parts := make([]string, voyage.DefaultDimension)
	for index := range parts {
		parts[index] = "0"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestRequestAndResponseLimitsFailBoundedly(t *testing.T) {
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy(), MaxRequestBytes: 2048, MaxResponseBytes: 64, MaxBatchItems: 2})
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeVectors(t, w, [][]float32{unitVector(0, 1)}, 1)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})
	authorizations := fullAuthorizations(t, policy)
	small := mediaInput(t, mediatest.WebP(2, 2))

	_, err = client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Text: strings.Repeat("x", 4096)}, {Media: small}}}}, authorizations)
	require.ErrorIs(t, err, voyage.ErrBatchTooLarge, "estimated request size exceeds the policy limit")

	_, err = client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: small}}}, {Parts: []voyage.Part{{Media: small}}}, {Parts: []voyage.Part{{Media: small}}}}, authorizations)
	require.ErrorIs(t, err, voyage.ErrBatchTooLarge, "batch exceeds the policy item limit")

	_, err = client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: small}}}}, authorizations)
	require.ErrorIs(t, err, voyage.ErrMalformedResponse, "oversized responses are refused")
	require.False(t, voyage.IsRetryable(err))
}

func TestHTTPFailureClassificationRetryAndSanitization(t *testing.T) {
	policy := testPolicy(t)
	tests := []struct {
		name       string
		statuses   []int
		body       string
		retryAfter string
		want       error
		wantCalls  int32
		retryable  bool
		wantDelay  bool
	}{
		{name: "unauthorized", statuses: []int{401}, body: `{"detail":"secret-token-echo"}`, want: voyage.ErrUnauthorized, wantCalls: 1},
		{name: "forbidden", statuses: []int{403}, want: voyage.ErrUnauthorized, wantCalls: 1},
		{name: "size rejection", statuses: []int{400}, body: `{"detail":"input is too large"}`, want: voyage.ErrBatchTooLarge, wantCalls: 1},
		{name: "other 400", statuses: []int{400}, body: `{"detail":"bad model"}`, want: voyage.ErrPermanentResponse, wantCalls: 1},
		{name: "422", statuses: []int{422}, want: voyage.ErrPermanentResponse, wantCalls: 1},
		{name: "rate limited then ok", statuses: []int{429, 200}, retryAfter: "0", wantCalls: 2, wantDelay: true},
		{name: "server error exhausted", statuses: []int{500, 502, 503}, want: voyage.ErrTransientResponse, wantCalls: 3, retryable: true},
		{name: "unexpected 2xx", statuses: []int{204}, want: voyage.ErrMalformedResponse, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := calls.Add(1)
				status := tt.statuses[min(int(call)-1, len(tt.statuses)-1)]
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				if status == 200 {
					writeVectors(t, w, [][]float32{unitVector(0, 1)}, 1)
					return
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 3, RetryBaseDelay: time.Millisecond})
			png := mediaInput(t, mediatest.PNG(2, 2, nil))
			result, err := client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, fullAuthorizations(t, policy))
			assert.Equal(t, tt.wantCalls, calls.Load())
			if tt.want == nil {
				require.NoError(t, err)
				assert.Equal(t, int(tt.wantCalls)-1, result.Metrics.Retries)
				return
			}
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, tt.retryable, voyage.IsRetryable(err))
			assert.NotContains(t, err.Error(), "secret-token-echo", "provider bodies never reach error strings")
			assert.NotContains(t, err.Error(), "bad model")
			assert.Equal(t, int(tt.wantCalls), voyage.MetricsFromError(err).Requests)
			var providerErr *voyage.ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, tt.statuses[len(tt.statuses)-1], providerErr.StatusCode)
		})
	}
}

func TestRetryAfterHeaderDrivesDelayAndIsExposed(t *testing.T) {
	policy := testPolicy(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 1})
	png := mediaInput(t, mediatest.PNG(2, 2, nil))
	_, err := client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, fullAuthorizations(t, policy))
	require.ErrorIs(t, err, voyage.ErrTransientResponse)
	delay, ok := voyage.RetryAfter(err)
	require.True(t, ok)
	assert.Equal(t, time.Second, delay)
	assert.Equal(t, int32(1), calls.Load())

	_, ok = voyage.RetryAfter(errors.New("plain"))
	assert.False(t, ok)
	assert.Equal(t, voyage.RequestMetrics{}, voyage.MetricsFromError(errors.New("plain")))
}

func TestTimeoutAndCancellation(t *testing.T) {
	policy := testPolicy(t)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	defer close(release)
	png := mediaInput(t, mediatest.PNG(2, 2, nil))

	t.Run("per attempt timeout is transient", func(t *testing.T) {
		client := newServerClient(t, server, policy, voyage.ClientConfig{Timeout: 50 * time.Millisecond, MaxRetries: 1})
		_, err := client.EmbedDocuments(t.Context(), []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, fullAuthorizations(t, policy))
		require.ErrorIs(t, err, voyage.ErrTransientResponse)
		assert.True(t, voyage.IsRetryable(err))
	})
	t.Run("caller cancellation is reported unchanged", func(t *testing.T) {
		client := newServerClient(t, server, policy, voyage.ClientConfig{Timeout: time.Minute, MaxRetries: 3})
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		_, err := client.EmbedDocuments(ctx, []voyage.Input{{Parts: []voyage.Part{{Media: png}}}}, fullAuthorizations(t, policy))
		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, voyage.IsRetryable(err))
	})
}
