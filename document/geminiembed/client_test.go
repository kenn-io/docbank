package geminiembed

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	"go.kenn.io/docbank/document/providerhttp"
)

// TestEmbedSendsOneTextRequestPerInputWithoutTaskType catches batching,
// request-shape, role-formatting, or result-order regressions in the Gemini
// text path.
func TestEmbedSendsOneTextRequestPerInputWithoutTaskType(t *testing.T) {
	profile := geminiTestProfile(t, 768)
	var requests [][]byte
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "https", request.URL.Scheme)
		assert.Equal(t, "generativelanguage.googleapis.com", request.URL.Host)
		assert.Equal(t, "/v1beta/models/gemini-embedding-2:embedContent", request.URL.Path)
		assert.Empty(t, request.URL.RawQuery)
		assert.Equal(t, "application/json", request.Header.Get("Accept"))
		assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
		assert.Equal(t, "synthetic-key", request.Header.Get("X-Goog-Api-Key"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requests = append(requests, body)
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(768)+`]},"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}}`), nil
	}))

	inputs := []document.EmbeddingInput{
		{Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"},
		{Key: "query-1", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "question"},
	}
	result, err := client.Embed(t.Context(), inputs, geminiAuthorization(profile.Descriptor))
	require.NoError(t, err)
	require.Len(t, requests, 2)
	if actual := string(requests[0]); actual != `{"model":"models/gemini-embedding-2","content":{"parts":[{"text":"title: none | text: passage"}]},"outputDimensionality":768}` {
		t.Errorf("document request body = %q", actual)
	}
	if actual := string(requests[1]); actual != `{"model":"models/gemini-embedding-2","content":{"parts":[{"text":"task: search result | query: question"}]},"outputDimensionality":768}` {
		t.Errorf("query request body = %q", actual)
	}
	for _, body := range requests {
		assert.NotContains(t, string(body), `"taskType"`)
		assert.NotContains(t, string(body), `"task_type"`)
	}
	require.Len(t, result.Vectors, 2)
	assert.Equal(t, "document-1", result.Vectors[0].Key)
	assert.Len(t, result.Vectors[0].Values, 768)
	assert.Equal(t, "query-1", result.Vectors[1].Key)
	assert.Len(t, result.Vectors[1].Values, 768)
}

// TestEmbedRejectsMalformedVectorAndUsageContracts catches response decoding
// that admits a malformed vector or untrusted numeric usage into a result or
// receipt.
func TestEmbedRejectsMalformedVectorAndUsageContracts(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "wrong dimension", body: `{"embedding":{"values":[1]},"usageMetadata":{"promptTokenCount":1,"totalTokenCount":1}}`},
		{name: "non-finite vector", body: `{"embedding":{"values":[1e999]}}`},
		{name: "negative usage", body: `{"embedding":{"values":[` + testVectorJSON(128) + `]},"usageMetadata":{"promptTokenCount":-1}}`},
		{name: "schema drift", body: `{"embedding":{"values":[` + testVectorJSON(128) + `]},"provider_private":"synthetic"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			profile := geminiTestProfile(t, 128)
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return geminiJSONResponse(request, testCase.body), nil
			}))
			_, err := client.Embed(t.Context(), []document.EmbeddingInput{{
				Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage",
			}}, geminiSingleAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "synthetic")
		})
	}
}

// TestEmbedNormalizesFiniteNonUnitVectorAndRejectsZero catches response
// validation that returns raw provider values despite the descriptor's fixed
// unit-length policy, or treats a zero vector as normalizable.
func TestEmbedNormalizesFiniteNonUnitVectorAndRejectsZero(t *testing.T) {
	t.Run("non-unit vector", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		values := make([]float32, 128)
		values[0], values[1] = 3, 4
		client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return geminiJSONResponse(request, `{"embedding":{"values":[`+vectorJSON(values)+`]}}`), nil
		}))

		result, err := client.Embed(t.Context(), []document.EmbeddingInput{{
			Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage",
		}}, geminiSingleAuthorization(profile.Descriptor))
		require.NoError(t, err)
		require.Len(t, result.Vectors, 1)
		require.Len(t, result.Vectors[0].Values, 128)
		assert.Equal(t, math.Float32bits(0.6), math.Float32bits(result.Vectors[0].Values[0]))
		assert.Equal(t, math.Float32bits(0.8), math.Float32bits(result.Vectors[0].Values[1]))
		assert.Equal(t, make([]float32, 126), result.Vectors[0].Values[2:])
	})

	t.Run("zero vector", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return geminiJSONResponse(request, `{"embedding":{"values":[`+vectorJSON(make([]float32, 128))+`]}}`), nil
		}))

		_, err := client.Embed(t.Context(), []document.EmbeddingInput{{
			Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage",
		}}, geminiSingleAuthorization(profile.Descriptor))
		require.ErrorIs(t, err, ErrPermanentResponse)
	})
}

func TestClearPreparedInputsZerosPayloadsAndFiles(t *testing.T) {
	payload := []byte("private rendered text")
	fileData := []byte("private file bytes")
	prepared := []preparedInput{{payload: payload}, {file: &verifiedFile{data: fileData}}}

	clearPreparedInputs(prepared)

	assert.Equal(t, make([]byte, len(payload)), payload)
	assert.Equal(t, make([]byte, len(fileData)), fileData)
}

// TestEmbedFailsLocallyBeforeCredentialsOrEgress catches a direct-file path
// that resolves a credential or starts an HTTP request before task 3 has
// authorized multimodal transport.
func TestEmbedFailsLocallyBeforeCredentialsOrEgress(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("egress must not start")
	}))
	_, err := client.Embed(t.Context(), []document.EmbeddingInput{{
		Key: "direct-file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile,
	}}, geminiSingleAuthorization(profile.Descriptor))
	require.Error(t, err)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

// TestEmbedWithReceiptRetainsOnlyBoundedProviderProvenance catches receipts
// that omit the execution identity or retain request and response content.
func TestEmbedWithReceiptRetainsOnlyBoundedProviderProvenance(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]},"usageMetadata":{"promptTokenCount":3,"cachedContentTokenCount":1,"totalTokenCount":4}}`)
		response.Header.Set("X-Goog-Request-Id", "provider-request-1")
		return response, nil
	}))
	execution, err := client.EmbedWithReceipt(t.Context(), []document.EmbeddingInput{{
		Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "private passage",
	}}, geminiSingleAuthorization(profile.Descriptor))
	require.NoError(t, err)
	assert.Equal(t, 1, execution.Receipt.RequestCount)
	assert.Equal(t, TransportInline, execution.Receipt.Transport)
	assert.Equal(t, int64(3), execution.Receipt.PromptTokens)
	assert.Equal(t, int64(1), execution.Receipt.CachedContentTokens)
	assert.Equal(t, int64(4), execution.Receipt.TotalTokens)
	assert.Equal(t, []string{"provider-request-1"}, execution.Receipt.ProviderResponseIDs)
	assert.Equal(t, "document-1", execution.Result.Vectors[0].Key)
}

func TestRecordReceiptResponseTruncatesProviderIDsWithoutFailing(t *testing.T) {
	receipt := Receipt{}
	for index := range 129 {
		require.True(t, recordReceiptResponse(&receipt, nil, fmt.Sprintf("provider-request-%03d", index)))
	}

	assert.Len(t, receipt.ProviderResponseIDs, 128)
	assert.Equal(t, 1, receipt.OmittedProviderResponseIDs)
	assert.NotContains(t, receipt.ProviderResponseIDs, "provider-request-128")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type countingSecrets struct {
	value string
	calls atomic.Int32
}

func (secrets *countingSecrets) ResolveSecret(context.Context, string) (string, error) {
	secrets.calls.Add(1)
	return secrets.value, nil
}

func geminiTestProfile(t *testing.T, dimension int) Profile {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile:         document.ModelInputProfileCustom,
		CompatibilityID: "gemini-embedding-2/search/v1",
		Document:        document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "title: none | text: {{content}}"},
		Query:           document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "task: search result | query: {{content}}"},
	})
	require.NoError(t, err)
	profile := Profile{
		CompatibilityEpoch: "gemini-embedding-2", SecretBinding: "secret:gemini", Transport: TransportInline,
		CapabilityProfileFingerprint: testCapabilityProfile, DisclosureFingerprint: testDisclosurePolicy,
		RequestTimeout: time.Second, MaxInputBytes: 4096, MaxRequestBytes: 8192, MaxResponseBytes: 16384,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "generativelanguage.googleapis.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, ProxyMode: providerhttp.ProxyDisabled,
			ConnectTimeout: time.Second, KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
		},
		Descriptor: document.EmbeddingDescriptor{
			ID: ProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			TrustBoundary: document.EmbeddingTrustHostedProvider, Model: "gemini-embedding-2",
			ModelRevision: "gemini-embedding-2", Dimension: dimension, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: ScalarEncodingFloat32,
			DocumentFormatter: DocumentFormatterV1, QueryFormatter: QueryFormatterV1,
			InputKinds: []document.EmbeddingInputKind{
				document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk,
			},
			CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
		},
	}
	fingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return profile
}

func newGeminiTestClient(t *testing.T, profile Profile, secrets SecretResolver, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(profile, secrets, syntheticResolver{netip.MustParseAddr("192.0.2.10")}, &http.Client{})
	require.NoError(t, err)
	client.http.Transport = transport
	return client
}

func geminiAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 2, MaxInputBytes: 64, MaxResponseBytes: 8192}
}

func geminiSingleAuthorization(descriptor document.EmbeddingDescriptor) document.EmbeddingAuthorization {
	authorization := geminiAuthorization(descriptor)
	authorization.MaxBatchItems = 1
	return authorization
}

func geminiJSONResponse(request *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func testVectorJSON(dimension int) string {
	values := make([]float32, dimension)
	values[0] = 1
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(bytes.Trim(encoded, "[]"))
}

func vectorJSON(values []float32) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(bytes.Trim(encoded, "[]"))
}
