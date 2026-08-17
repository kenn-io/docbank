package mistral

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientProcessStreamsVerifiedDocumentAndReturnsNeutralEvidence(t *testing.T) {
	content := []byte("%PDF-1.7\nsynthetic")
	policy := testPolicy(t, 1024, 10)
	prepared := prepareTestDocument(t, policy, content)
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)

	var requestBody []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/v1/ocr", request.URL.Path)
		assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
		var readErr error
		requestBody, readErr = io.ReadAll(request.Body)
		assert.NoError(t, readErr)
		assert.Equal(t, request.ContentLength, int64(len(requestBody)))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, writeErr := io.WriteString(w, `{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"# Synthetic","header":"Header","footer":"Footer","dimensions":{"dpi":144,"height":792,"width":612}}],"usage_info":{"pages_processed":1,"doc_size_bytes":18}}`)
		assert.NoError(t, writeErr)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{APIKey: "synthetic-key"})

	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	assert.Equal(t, "mistral-ocr-4-0", result.ReturnedModel)
	assert.Equal(t, 1, result.UnitsProcessed)
	require.NotNil(t, result.ProviderBytes)
	assert.Equal(t, int64(len(content)), *result.ProviderBytes)
	assert.Equal(t, "pdf", result.Document.Family)
	assert.Equal(t, "page", result.Document.UnitKind)
	require.Len(t, result.Document.Units, 1)
	assert.Equal(t, "# Synthetic", result.Document.Units[0].Markdown)
	assert.Equal(t, 144, result.Document.Units[0].Dimensions.DPI)
	assert.Equal(t, RequestMetrics{Requests: 1, Retries: 0, Latency: result.Metrics.Latency}, result.Metrics)
	assert.Positive(t, result.Metrics.Latency)

	var decoded struct {
		Model    string `json:"model"`
		Document struct {
			Type string `json:"type"`
			URL  string `json:"document_url"`
		} `json:"document"`
		ExtractHeader bool   `json:"extract_header"`
		ExtractFooter bool   `json:"extract_footer"`
		Pages         string `json:"pages"`
	}
	require.NoError(t, json.Unmarshal(requestBody, &decoded))
	assert.Equal(t, defaultModel, decoded.Model)
	assert.Equal(t, "document_url", decoded.Document.Type)
	assert.True(t, decoded.ExtractHeader)
	assert.True(t, decoded.ExtractFooter)
	assert.Equal(t, "0-9", decoded.Pages)
	encoded := strings.TrimPrefix(decoded.Document.URL, "data:"+mediaTypePDF+";base64,")
	uploaded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	assert.Equal(t, content, uploaded)
}

func TestClientRejectsChangedReleasedAndCrossPolicyDocumentsBeforeUpload(t *testing.T) {
	policy := testPolicy(t, 1024, 10)
	manifest := syntheticManifest(t, policy, true)
	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	clientWithoutRequests := func(policy Policy) *Client {
		client, clientErr := NewClient(policy, ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("unexpected provider request")
			}),
		}})
		require.NoError(t, clientErr)
		return client
	}
	client := clientWithoutRequests(policy)

	changed := prepareTestDocument(t, policy, []byte("%PDF-1.7\nfirst"))
	require.NoError(t, os.WriteFile(changed.path, []byte("%PDF-1.7\nother"), 0o600))
	_, err = client.Process(t.Context(), changed, authorization)
	require.ErrorContains(t, err, "hash mismatch")

	if runtime.GOOS != "windows" {
		public := prepareTestDocument(t, policy, []byte("%PDF-1.7\nprivate"))
		require.NoError(t, os.Chmod(public.path, 0o644))
		_, err = client.Process(t.Context(), public, authorization)
		require.ErrorContains(t, err, "permissions must be private")
	}

	released := prepareTestDocument(t, policy, []byte("%PDF-1.7\nrelease"))
	require.NoError(t, released.Release())
	_, err = client.Process(t.Context(), released, authorization)
	require.ErrorContains(t, err, "released")

	otherPolicy := testPolicy(t, 1024, 9)
	_, err = client.Process(t.Context(), prepareTestDocument(t, policy, []byte("%PDF-1.7\npolicy")), FormatAuthorization{
		format: authorization.format, method: authorization.method,
		policyFingerprint: authorization.policyFingerprint, policyDigest: otherPolicy.digest,
	})
	require.ErrorContains(t, err, "different policy")

	smallPolicy := testPolicy(t, 8, 10)
	client = clientWithoutRequests(smallPolicy)
	_, err = client.Process(t.Context(), prepareTestDocument(t, policy, []byte("%PDF-1.7\nlarge")), FormatAuthorization{
		format: authorization.format, method: authorization.method,
		policyFingerprint: authorization.policyFingerprint, policyDigest: smallPolicy.digest,
	})
	require.ErrorContains(t, err, "policy limit 8")
}

func TestClientClassifiesResponsesAndDoesNotFollowRedirects(t *testing.T) {
	policy := testPolicy(t, 1024, 10)
	prepared := prepareTestDocument(t, policy, []byte("%PDF-1.7\nx"))
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)

	tests := []struct {
		name        string
		status      int
		response    string
		contentType string
		maxBytes    int64
		wantError   string
	}{
		{name: "permanent", status: http.StatusBadRequest, response: "private response body", wantError: ErrPermanentResponse.Error()},
		{name: "too large", status: http.StatusOK, response: strings.Repeat("x", 65), contentType: "application/json", maxBytes: 64, wantError: ErrResponseTooLarge.Error()},
		{name: "missing index", status: http.StatusOK, response: `{"model":"mistral-ocr-4-0","pages":[{}],"usage_info":{"pages_processed":1}}`, contentType: "application/json", wantError: "invalid index"},
		{name: "usage mismatch", status: http.StatusOK, response: `{"model":"mistral-ocr-4-0","pages":[{"index":0}],"usage_info":{"pages_processed":0}}`, contentType: "application/json", wantError: "processed 0 units but returned 1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.contentType != "" {
					w.Header().Set("Content-Type", test.contentType)
				}
				w.WriteHeader(test.status)
				_, writeErr := io.WriteString(w, test.response)
				assert.NoError(t, writeErr)
			}))
			defer server.Close()
			casePolicy := policy
			caseAuthorization := authorization
			if test.maxBytes > 0 {
				values := policy.Values()
				casePolicy = testPolicy(t, values.MaxDocumentBytes, values.MaxUnits)
				casePolicy.values.MaxResponseBytes = test.maxBytes
				casePolicy.digest, err = policyValuesDigest(casePolicy.values)
				require.NoError(t, err)
				caseAuthorization.policyDigest = casePolicy.digest
			}
			client := newServerClient(t, server, casePolicy, ClientConfig{})
			_, err := client.Process(t.Context(), prepared, caseAuthorization)
			require.ErrorContains(t, err, test.wantError)
			assert.NotContains(t, err.Error(), "private response body")
		})
	}

	targetCalled := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalled = true }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := newServerClient(t, redirect, policy, ClientConfig{})
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorContains(t, err, "unexpected HTTP 307")
	assert.False(t, targetCalled)
}

func TestClientRetriesOnlyTransientWorkAndReverifiesEveryAttempt(t *testing.T) {
	policy := testPolicy(t, 1024, 10)
	prepared := prepareTestDocument(t, policy, []byte("%PDF-1.7\noriginal"))
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		_, copyErr := io.Copy(io.Discard, request.Body)
		assert.NoError(t, copyErr)
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, writeErr := io.WriteString(w, `{"model":"mistral-ocr-4-0","pages":[],"usage_info":{"pages_processed":0}}`)
		assert.NoError(t, writeErr)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{MaxRetryDelay: time.Millisecond})
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	assert.Equal(t, 2, requests)
	assert.Equal(t, 2, result.Metrics.Requests)
	assert.Equal(t, 1, result.Metrics.Retries)

	requests = 0
	mutating := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		_, copyErr := io.Copy(io.Discard, request.Body)
		assert.NoError(t, copyErr)
		assert.NoError(t, os.WriteFile(prepared.path, []byte("%PDF-1.7\nmodified"), 0o600))
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer mutating.Close()
	client = newServerClient(t, mutating, policy, ClientConfig{MaxRetryDelay: time.Millisecond})
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorContains(t, err, "hash mismatch")
	assert.Equal(t, 1, requests)
}

func TestClientPreservesAccountingWhenRetriesExhaustOrWaitIsCanceled(t *testing.T) {
	policy := testPolicy(t, 1024, 10)
	prepared := prepareTestDocument(t, policy, []byte("%PDF-1.7\nx"))
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)

	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		_, copyErr := io.Copy(io.Discard, request.Body)
		assert.NoError(t, copyErr)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{MaxRetries: 1, MaxRetryDelay: time.Millisecond})
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorIs(t, err, ErrTransientResponse)
	assert.Equal(t, 2, requests)
	metrics := MetricsFromError(err)
	assert.Equal(t, 2, metrics.Requests)
	assert.Equal(t, 1, metrics.Retries)
	assert.Positive(t, metrics.Latency)

	ctx, cancel := context.WithCancel(t.Context())
	requests = 0
	canceling := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		_, copyErr := io.Copy(io.Discard, request.Body)
		assert.NoError(t, copyErr)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		cancel()
	}))
	defer canceling.Close()
	client = newServerClient(t, canceling, policy, ClientConfig{})
	_, err = client.Process(ctx, prepared, authorization)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, requests)
	metrics = MetricsFromError(err)
	assert.Equal(t, 1, metrics.Requests)
	assert.Equal(t, 0, metrics.Retries)
	assert.Positive(t, metrics.Latency)
}

func TestClientRejectsProviderUnitContractDrift(t *testing.T) {
	policy := testPolicy(t, 1024, 3)
	prepared := prepareTestDocument(t, policy, []byte("%PDF-1.7\nx"))
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, writeErr := io.WriteString(w, `{"model":"mistral-ocr-4-0","pages":[{"index":0},{"index":1},{"index":2},{"index":3}],"usage_info":{"pages_processed":4}}`)
		assert.NoError(t, writeErr)
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{})
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
}

func TestLocalExactRequiresProviderEquality(t *testing.T) {
	policy := testPolicy(t, 1024, 10)
	prepared := prepareTestDocument(t, policy, []byte("%PDF-1.7\nx"))
	prepared.localUnits = 2
	format, found := CandidateFormatByID("pdf")
	require.True(t, found)
	authorization := FormatAuthorization{format: format, method: UnitBoundLocalExact, policyDigest: policy.digest}
	for _, processed := range []int{1, 3} {
		t.Run(strconv.Itoa(processed), func(t *testing.T) {
			pages := make([]map[string]int, processed)
			for index := range pages {
				pages[index] = map[string]int{"index": index}
			}
			body, err := json.Marshal(map[string]any{
				"model": defaultModel, "pages": pages,
				"usage_info": map[string]int{"pages_processed": processed},
			})
			require.NoError(t, err)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, writeErr := w.Write(body)
				assert.NoError(t, writeErr)
			}))
			defer server.Close()
			client := newServerClient(t, server, policy, ClientConfig{})
			_, err = client.Process(t.Context(), prepared, authorization)
			require.ErrorIs(t, err, ErrCapabilityContract)
		})
	}
}

func newServerClient(t *testing.T, server *httptest.Server, policy Policy, config ClientConfig) *Client {
	t.Helper()
	if config.APIKey == "" {
		config.APIKey = "synthetic-key"
	}
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	base := server.Client()
	transport := base.Transport
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		return transport.RoundTrip(clone)
	})}
	client, err := NewClient(policy, config)
	require.NoError(t, err)
	return client
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	_, err := NewClient(testPolicy(t, 1024, 10), ClientConfig{})
	require.ErrorContains(t, err, "API key is required")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
