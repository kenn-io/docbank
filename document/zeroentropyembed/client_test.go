package zeroentropyembed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"io"
	"math"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmbedSeparatesInputTypesAndPreservesInputOrder(t *testing.T) {
	profile := testProfile(t, 40, EncodingFloat, LatencyFast)
	var calls atomic.Int32
	client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "https://api.zeroentropy.dev/v1/models/embed", request.URL.String())
		assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var payload struct {
			Model          string         `json:"model"`
			InputType      string         `json:"input_type"`
			Input          []string       `json:"input"`
			Dimensions     int            `json:"dimensions"`
			EncodingFormat EncodingFormat `json:"encoding_format"`
			Latency        Latency        `json:"latency"`
		}
		require.NoError(t, json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)))
		assert.Equal(t, Model, payload.Model)
		assert.Equal(t, 40, payload.Dimensions)
		assert.Equal(t, EncodingFloat, payload.EncodingFormat)
		assert.Equal(t, LatencyFast, payload.Latency)
		value := float64(calls.Add(1))
		vector := make([]float64, 40)
		vector[0] = value
		response, err := json.Marshal(map[string]any{
			"results": []any{map[string]any{"embedding": vector}},
			"usage":   map[string]any{"total_bytes": 155, "total_tokens": 2},
		})
		require.NoError(t, err)
		if payload.InputType == "document" {
			assert.Equal(t, []string{"first passage"}, payload.Input)
		} else {
			assert.Equal(t, "query", payload.InputType)
			assert.Equal(t, []string{"search words"}, payload.Input)
		}
		return jsonResponse(request, http.StatusOK, response), nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first passage"},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "search words"},
	}

	execution, err := client.EmbedWithReceipt(context.Background(), inputs, authorization(client.descriptor, 2))
	require.NoError(t, err)
	require.Len(t, execution.Result.Vectors, 2)
	assert.Equal(t, "document", execution.Result.Vectors[0].Key)
	assert.InDelta(t, 1, execution.Result.Vectors[0].Values[0], 0)
	assert.Equal(t, "query", execution.Result.Vectors[1].Key)
	assert.InDelta(t, 2, execution.Result.Vectors[1].Values[0], 0)
	assert.Equal(t, Receipt{ProviderID: ProviderID, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint: client.descriptor.PolicyFingerprint, Model: Model, ModelRevision: profile.Descriptor.ModelRevision,
		EncodingFormat: EncodingFloat, RequestedLatency: LatencyFast, RequestCount: 2,
		TotalBytes: 310, TotalTokens: 4}, execution.Receipt)
}

func TestEmbedDecodesExactLittleEndianBase64(t *testing.T) {
	profile := testProfile(t, 40, EncodingBase64, LatencyAuto)
	raw := make([]byte, 40*4)
	for index := range 40 {
		binary.LittleEndian.PutUint32(raw[index*4:], math.Float32bits(float32(index)+0.25))
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(body), `"latency"`)
		return jsonResponse(request, http.StatusOK, []byte(`{"results":[{"embedding":"`+encoded+`"}],"usage":{"total_bytes":155,"total_tokens":2}}`)), nil
	}))

	result, err := client.Embed(context.Background(), oneInput(), authorization(client.descriptor, 1))
	require.NoError(t, err)
	require.Len(t, result.Vectors, 1)
	assert.InDelta(t, 0.25, result.Vectors[0].Values[0], 0)
	assert.InDelta(t, 39.25, result.Vectors[0].Values[39], 0)
}

func TestEmbedRejectsResponseDriftAndInvalidVectors(t *testing.T) {
	validVector := strings.TrimSuffix(strings.Repeat("0,", 40), ",")
	valid := `{"results":[{"embedding":[` + validVector + `]}],"usage":{"total_bytes":155,"total_tokens":2}}`
	tests := map[string]string{
		"missing result":  `{"results":[],"usage":{"total_bytes":155,"total_tokens":2}}`,
		"wrong dimension": strings.Replace(valid, validVector, "0,0", 1),
		"non finite":      strings.Replace(valid, "0,", "1e1000,", 1),
		"missing usage":   strings.Replace(valid, `,"usage":{"total_bytes":155,"total_tokens":2}`, "", 1),
		"negative usage":  strings.Replace(valid, `"total_tokens":2`, `"total_tokens":-1`, 1),
		"unknown field":   strings.Replace(valid, `"usage":`, `"private":true,"usage":`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			profile := testProfile(t, 40, EncodingFloat, LatencySlow)
			client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(body)), nil
			}))
			_, err := client.Embed(context.Background(), oneInput(), authorization(client.descriptor, 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestEmbedRejectsProviderPayloadLimitBeforeSecretsOrEgress(t *testing.T) {
	profile := testProfile(t, 40, EncodingFloat, LatencyFast)
	profile.MaxInputItemBytes = maximumInputBytes
	profile.MaxInputBytes = maximumInputBytes
	profile.Descriptor = descriptorFor(t, profile)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, assert.AnError
	}))
	input := oneInput()
	input[0].Text = strings.Repeat("a", int(maximumInputBytes-100))
	authorization := authorization(client.descriptor, 1)
	authorization.MaxInputBytes = maximumInputBytes

	_, err := client.Embed(context.Background(), input, authorization)
	require.ErrorIs(t, err, ErrCapacityResponse)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestEmbedPreservesCancellationDuringResponseRead(t *testing.T) {
	started := make(chan struct{})
	profile := testProfile(t, 40, EncodingFloat, LatencyFast)
	client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: &contextBody{ctx: request.Context()}, Request: request}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, oneInput(), authorization(client.descriptor, 1))
		done <- err
	}()
	<-started
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

func TestRetryAfterClampsBeforeDurationConversion(t *testing.T) {
	err := statusError(http.StatusTooManyRequests, "9223372036854775807", time.Now().UTC())
	delay, ok := RetryAfter(err)
	assert.True(t, ok)
	assert.Equal(t, time.Hour, delay)
}

func oneInput() []document.EmbeddingInput {
	return []document.EmbeddingInput{{Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "alpha"}}
}

func authorization(descriptor document.EmbeddingDescriptor, batch int) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: batch, MaxInputBytes: 4096,
		MaxResponseBytes: int64(batch*descriptor.Dimension*4 + 1024)}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testClient(t *testing.T, profile Profile, secrets SecretResolver, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(profile, secrets, testResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	client.http.Transport = transport
	return client
}

func jsonResponse(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: request}
}

type countingSecrets struct {
	value string
	calls atomic.Int32
}

func (resolver *countingSecrets) ResolveSecret(context.Context, string) (string, error) {
	resolver.calls.Add(1)
	return resolver.value, nil
}

type contextBody struct{ ctx context.Context }

func (body *contextBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*contextBody) Close() error { return nil }
