package cohereembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"image/color"
	"io"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestEmbedSendsExactRoleAndImageRequestsAndRestoresCallerOrder(t *testing.T) {
	profile := testProfile(t, 256)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, profile, secrets, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "https://api.cohere.com/v2/embed", request.URL.String())
		assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
		assert.Equal(t, "application/json", request.Header.Get("Accept"))
		assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var payload struct {
			Model  string   `json:"model"`
			Texts  []string `json:"texts"`
			Inputs []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"inputs"`
			InputType       string   `json:"input_type"`
			EmbeddingTypes  []string `json:"embedding_types"`
			Truncate        string   `json:"truncate"`
			OutputDimension int      `json:"output_dimension"`
		}
		require.NoError(t, json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)))
		assert.Equal(t, Model, payload.Model)
		assert.Equal(t, []string{"float"}, payload.EmbeddingTypes)
		assert.Equal(t, "NONE", payload.Truncate)
		assert.Equal(t, 256, payload.OutputDimension)
		vector := make([]float32, 256)
		switch {
		case len(payload.Inputs) == 1:
			assert.Empty(t, payload.Texts)
			assert.Equal(t, "search_document", payload.InputType)
			require.Len(t, payload.Inputs[0].Content, 1)
			assert.Equal(t, "image_url", payload.Inputs[0].Content[0].Type)
			assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(tinyPNG(t)), payload.Inputs[0].Content[0].ImageURL.URL)
			vector[2] = 1
		case payload.InputType == "search_document":
			assert.Equal(t, []string{"document text"}, payload.Texts)
			assert.Empty(t, payload.Inputs)
			vector[0] = 1
		case payload.InputType == "search_query":
			assert.Equal(t, []string{"query text"}, payload.Texts)
			assert.Empty(t, payload.Inputs)
			vector[1] = 1
		default:
			t.Fatalf("unexpected request: %#v", payload)
		}
		responseValue := map[string]any{"id": "synthetic-id", "embeddings": map[string]any{"float": [][]float32{vector}}}
		if len(payload.Inputs) != 0 {
			responseValue["images"] = []map[string]any{{"width": 1, "height": 1, "format": "png", "bit_depth": 8}}
			responseValue["response_type"] = "embeddings_by_type"
			responseValue["meta"] = map[string]any{"billed_units": map[string]any{"images": 1}}
		}
		response, err := json.Marshal(responseValue)
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, response), nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "document text"},
		{Key: "image", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: imageUpload(t, tinyPNG(t), "image/png")},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "query text"},
	}
	original := slices.Clone(inputs)
	execution, err := client.EmbedWithReceipt(context.Background(), inputs, authorization(client.Descriptor(), len(inputs)))
	require.NoError(t, err)
	result := execution.Result
	assert.Equal(t, int32(3), requests.Load())
	assert.Equal(t, int32(1), secrets.calls.Load())
	assert.Equal(t, original, inputs)
	require.Len(t, result.Vectors, 3)
	assert.Equal(t, "document", result.Vectors[0].Key)
	assert.InDelta(t, 1, result.Vectors[0].Values[0], 0)
	assert.Equal(t, "image", result.Vectors[1].Key)
	assert.InDelta(t, 1, result.Vectors[1].Values[2], 0)
	assert.Equal(t, "query", result.Vectors[2].Key)
	assert.InDelta(t, 1, result.Vectors[2].Values[1], 0)
	assert.Equal(t, Receipt{ProviderID: ProviderID, DescriptorFingerprint: client.Descriptor().Fingerprint,
		PolicyFingerprint: client.Descriptor().PolicyFingerprint, Model: Model,
		ModelRevision: "deployment-2026-08", RequestCount: 3, ImageInputs: 1, BilledImages: 1,
		ProviderResponseIDs: []string{"synthetic-id", "synthetic-id", "synthetic-id"}}, execution.Receipt)
}

func TestEmbedRejectsImageOverPerItemBoundBeforeSecretOrRequest(t *testing.T) {
	data := tinyPNG(t)
	profile := testProfile(t, 256)
	profile.MaxInputItemBytes = int64(len(data) - 1)
	profile.Descriptor = descriptorFor(t, profile)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))
	first := imageUpload(t, data, "image/png")
	second := imageUpload(t, data, "image/png")
	inputs := []document.EmbeddingInput{
		{Key: "image-one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: first},
		{Key: "image-two", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: second},
	}

	_, err := client.Embed(context.Background(), inputs, authorization(client.Descriptor(), len(inputs)))
	require.ErrorIs(t, err, ErrCapacityResponse)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
	assert.True(t, first.closed)
	assert.True(t, second.closed)
}

func TestEmbedRejectsUnboundedProviderUsage(t *testing.T) {
	profile := testProfile(t, 256)
	client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		vector := make([]float32, 256)
		vector[0] = 1
		body, err := json.Marshal(map[string]any{
			"id": "synthetic-id", "embeddings": map[string]any{"float": [][]float32{vector}},
			"meta": map[string]any{"billed_units": map[string]any{"input_tokens": int64(math.MaxInt64)}},
		})
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, body), nil
	}))
	input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "document text"}

	_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
	require.ErrorIs(t, err, ErrPermanentResponse)
}

func TestEmbedAcceptsDocumentedFractionalUsageAndPreservesItInReceipt(t *testing.T) {
	profile := testProfile(t, 256)
	client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		vector := make([]float32, 256)
		body, err := json.Marshal(map[string]any{
			"id": "synthetic-id", "embeddings": map[string]any{"float": [][]float32{vector}},
			"meta": map[string]any{
				"billed_units": map[string]any{"images": 0.0, "input_tokens": 1.5, "image_tokens": 0.0,
					"output_tokens": 3.5, "search_units": 4.25, "classifications": 5.5, "pages": 6.75},
				"tokens":        map[string]any{"input_tokens": 1.5, "output_tokens": 3.5},
				"cached_tokens": 0.75,
			},
		})
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, body), nil
	}))
	input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "document text"}

	execution, err := client.EmbedWithReceipt(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
	require.NoError(t, err)
	assert.InDelta(t, 1.5, execution.Receipt.InputTokens, 0)
	assert.Zero(t, execution.Receipt.ImageTokens)
	assert.InDelta(t, 3.5, execution.Receipt.OutputTokens, 0)
	assert.InDelta(t, 4.25, execution.Receipt.SearchUnits, 0)
	assert.InDelta(t, 5.5, execution.Receipt.Classifications, 0)
	assert.InDelta(t, 6.75, execution.Receipt.Pages, 0)
	assert.InDelta(t, 0.75, execution.Receipt.CachedTokens, 0)
	assert.Zero(t, execution.Receipt.BilledImages)
}

func TestEmbedRejectsNonzeroImageTokensForTextRequest(t *testing.T) {
	profile := testProfile(t, 256)
	client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		vector := make([]float32, 256)
		body, err := json.Marshal(map[string]any{"id": "synthetic-id", "embeddings": map[string]any{"float": [][]float32{vector}},
			"meta": map[string]any{"billed_units": map[string]any{"image_tokens": 2.25}}})
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, body), nil
	}))
	input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "document text"}

	_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
	require.ErrorIs(t, err, ErrPermanentResponse)
}

func TestEmbedPreservesFractionalImageTokensForImageRequest(t *testing.T) {
	profile := testProfile(t, 256)
	client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		vector := make([]float32, 256)
		body, err := json.Marshal(map[string]any{"id": "synthetic-id", "embeddings": map[string]any{"float": [][]float32{vector}},
			"images": []map[string]any{{"width": 1, "height": 1, "format": "png", "bit_depth": 8}},
			"meta":   map[string]any{"billed_units": map[string]any{"images": 1.0, "image_tokens": 2.25}}})
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, body), nil
	}))
	input := document.EmbeddingInput{Key: "image", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputOriginalFile, Source: imageUpload(t, tinyPNG(t), "image/png")}

	execution, err := client.EmbedWithReceipt(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
	require.NoError(t, err)
	assert.InDelta(t, 2.25, execution.Receipt.ImageTokens, 0)
}

func TestEmbedRejectsContradictoryDocumentedUsage(t *testing.T) {
	profile := testProfile(t, 256)
	client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		vector := make([]float32, 256)
		body, err := json.Marshal(map[string]any{
			"id": "synthetic-id", "embeddings": map[string]any{"float": [][]float32{vector}},
			"meta": map[string]any{"billed_units": map[string]any{"input_tokens": 1.5},
				"tokens": map[string]any{"input_tokens": 2.5}},
		})
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, body), nil
	}))
	input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "document text"}

	_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
	require.ErrorIs(t, err, ErrPermanentResponse)
}

func TestEmbedAcceptsOnlyLocallyVerifiedCohereImageFormats(t *testing.T) {
	tests := []struct {
		name, mediaType, format string
		data                    []byte
	}{
		{name: "jpeg", mediaType: "image/jpeg", format: "jpeg", data: mediatest.JPEG(2, 2, color.Black)},
		{name: "png", mediaType: "image/png", format: "png", data: mediatest.PNG(2, 2, color.Black)},
		{name: "webp", mediaType: "image/webp", format: "webp", data: mediatest.WebP(2, 2)},
		{name: "gif", mediaType: "image/gif", format: "gif", data: mediatest.GIF(2, 2, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				vector := make([]float32, 256)
				body, err := json.Marshal(map[string]any{"id": "synthetic", "embeddings": map[string]any{"float": [][]float32{vector}},
					"images": []map[string]any{{"width": 2, "height": 2, "format": test.format, "bit_depth": 8}}})
				require.NoError(t, err)
				return jsonResponse(request, http.StatusOK, body), nil
			}))
			source := imageUpload(t, test.data, test.mediaType)
			input := document.EmbeddingInput{Key: "image", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputOriginalFile, Source: source}
			_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
			require.NoError(t, err)
			assert.True(t, source.closed)
		})
	}
}

func TestEmbedClosesEverySourceAndMakesNoEgressWhenSourceAuthorityFails(t *testing.T) {
	first := imageUpload(t, tinyPNG(t), "image/png")
	first.metadata.SHA256 = strings.Repeat("0", 64)
	second := imageUpload(t, tinyPNG(t), "image/png")
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, testProfile(t, 256), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))
	inputs := []document.EmbeddingInput{
		{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: first},
		{Key: "two", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: second},
	}

	_, err := client.Embed(context.Background(), inputs, authorization(client.Descriptor(), len(inputs)))
	require.Error(t, err)
	assert.True(t, first.closed)
	assert.True(t, second.closed)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestEmbedPreservesCancellationWhileClosingEveryImageSource(t *testing.T) {
	first := imageUpload(t, tinyPNG(t), "image/png")
	second := imageUpload(t, tinyPNG(t), "image/png")
	secrets := &countingSecrets{value: "synthetic-key"}
	client := testClient(t, testProfile(t, 256), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request must not run")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Embed(ctx, []document.EmbeddingInput{
		{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: first},
		{Key: "two", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: second},
	}, authorization(client.Descriptor(), 2))
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, first.closed)
	assert.True(t, second.closed)
	assert.Zero(t, secrets.calls.Load())
}

func TestEmbedRejectsStrictResponseDrift(t *testing.T) {
	vector := make([]float32, 256)
	valid, err := json.Marshal(map[string]any{"id": "synthetic", "embeddings": map[string]any{"float": [][]float32{vector}}})
	require.NoError(t, err)
	tests := map[string]string{
		"unknown root":    strings.TrimSuffix(string(valid), "}") + `,"private":"value"}`,
		"missing id":      `{"embeddings":{"float":[` + vectorJSON() + `]}}`,
		"missing vector":  `{"id":"synthetic","embeddings":{"float":[]}}`,
		"wrong dimension": `{"id":"synthetic","embeddings":{"float":[[0]]}}`,
		"non-finite":      `{"id":"synthetic","embeddings":{"float":[[` + strings.Repeat("0,", 255) + `1e999]]}}`,
		"negative usage":  `{"id":"synthetic","embeddings":{"float":[` + vectorJSON() + `]},"meta":{"tokens":{"input_tokens":-1}}}`,
		"unknown meta":    `{"id":"synthetic","embeddings":{"float":[` + vectorJSON() + `]},"meta":{"private":1}}`,
		"unsafe id":       `{"id":"private response id","embeddings":{"float":[` + vectorJSON() + `]}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(body)), nil
			}))
			input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputRenditionChunk, Text: "private document"}
			_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestEmbedAcceptsDocumentedEmptyUnrequestedRepresentations(t *testing.T) {
	client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := []byte(`{"id":"synthetic","embeddings":{"float":[` + vectorJSON() + `],"int8":null,"uint8":[],"binary":null,"ubinary":[],"base64":[]}}`)
		return jsonResponse(request, http.StatusOK, body), nil
	}))
	input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: "private document"}

	_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
	require.NoError(t, err)
}

func TestEmbedRejectsNonemptyUnrequestedRepresentations(t *testing.T) {
	for name, representation := range map[string]string{
		"int8":    `"int8":[[1]]`,
		"uint8":   `"uint8":[[1]]`,
		"binary":  `"binary":[[1]]`,
		"ubinary": `"ubinary":[[1]]`,
		"base64":  `"base64":["AA=="]`,
	} {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := []byte(`{"id":"synthetic","embeddings":{"float":[` + vectorJSON() + `],` + representation + `}}`)
				return jsonResponse(request, http.StatusOK, body), nil
			}))
			input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputRenditionChunk, Text: "private document"}

			_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
		})
	}
}

func TestEmbedClassifiesSanitizedHTTPFailuresAndRetryAfter(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: http.StatusRequestTimeout, want: ErrTransientResponse},
		{status: http.StatusTooManyRequests, want: ErrTransientResponse},
		{status: http.StatusInternalServerError, want: ErrTransientResponse},
		{status: http.StatusRequestEntityTooLarge, want: ErrCapacityResponse},
		{status: http.StatusBadRequest, want: ErrPermanentResponse},
		{status: http.StatusTemporaryRedirect, want: ErrPermanentResponse},
	}
	for _, test := range tests {
		client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := jsonResponse(request, test.status, []byte(`{"private":"provider body"}`))
			response.Header.Set("Retry-After", "7200")
			return response, nil
		}))
		input := document.EmbeddingInput{Key: "document", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputRenditionChunk, Text: "private document"}
		_, err := client.Embed(context.Background(), []document.EmbeddingInput{input}, authorization(client.Descriptor(), 1))
		require.ErrorIs(t, err, test.want)
		assert.NotContains(t, err.Error(), "provider body")
		if test.status == http.StatusTooManyRequests {
			delay, ok := RetryAfter(err)
			assert.True(t, ok)
			assert.Equal(t, time.Hour, delay)
		}
	}
}

func vectorJSON() string {
	return "[" + strings.TrimSuffix(strings.Repeat("0,", 256), ",") + "]"
}

type countingSecrets struct {
	value string
	err   error
	calls atomic.Int32
}

func (resolver *countingSecrets) ResolveSecret(context.Context, string) (string, error) {
	resolver.calls.Add(1)
	return resolver.value, resolver.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type upload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
	closed   bool
}

func (value *upload) Close() error                                { value.closed = true; return nil }
func (value *upload) Metadata() document.AuthorizedUploadMetadata { return value.metadata }

func imageUpload(t *testing.T, data []byte, mediaType string) *upload {
	t.Helper()
	digest := sha256.Sum256(data)
	return &upload{Reader: bytes.NewReader(data), metadata: document.AuthorizedUploadMetadata{
		Filename: "synthetic.png", MediaFamily: "image", MediaType: mediaType,
		ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("a", 64),
		ProviderMetadataChecksum: strings.Repeat("b", 64),
		InputKind:                document.RenditionInputOriginalFile,
	}}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	require.NoError(t, err)
	return data
}

func authorization(descriptor document.EmbeddingDescriptor, batch int) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID,
		DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
		MaxBatchItems: batch, MaxInputBytes: 20 << 20, MaxResponseBytes: 1 << 20}
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
