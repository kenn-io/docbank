package zeroentropyrerank

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/retrieval"
)

func TestRerankSendsOnlyScopedExcerptsAndMapsEveryStableID(t *testing.T) {
	profile := testProfile(LatencyFast)
	client := testClient(t, profile, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "https://api.zeroentropy.dev/v1/models/rerank", request.URL.String())
		assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		for _, localOnly := range []string{"vault-private", "version-private", "build-private", "segment-private"} {
			assert.NotContains(t, string(body), localOnly)
		}
		var payload struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
			Latency   Latency  `json:"latency"`
		}
		require.NoError(t, json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)))
		assert.Equal(t, Model, payload.Model)
		assert.Equal(t, "private query", payload.Query)
		assert.Equal(t, []string{"first excerpt", "second excerpt"}, payload.Documents)
		assert.Equal(t, 2, payload.TopN)
		assert.Equal(t, LatencyFast, payload.Latency)
		response := []byte(`{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.2}],"total_bytes":360,"total_tokens":12,"actual_latency_mode":"fast","e2e_latency":0.25,"inference_latency":0.2}`)
		return rerankJSONResponse(request, http.StatusOK, response), nil
	}))
	request := rerankingRequest()

	execution, err := client.RerankWithReceipt(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, []retrieval.RerankScore{
		{Document: request.Candidates[0].Document, Score: 0.2},
		{Document: request.Candidates[1].Document, Score: 0.9},
	}, execution.Scores)
	assert.Equal(t, Receipt{PolicyFingerprint: client.PolicyFingerprint(), Model: Model,
		ModelRevision: profile.ModelRevision, RequestedLatency: LatencyFast, ActualLatency: LatencyFast,
		CandidateCount: 2, TotalBytes: 360, TotalTokens: 12, E2ELatencySeconds: 0.25,
		InferenceLatencySeconds: 0.2}, execution.Receipt)
}

func TestRerankOmitsAutoLatencyAndRejectsIncompleteOrInvalidResults(t *testing.T) {
	valid := `{"results":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.9}],"total_bytes":360,"total_tokens":12,"actual_latency_mode":"slow","e2e_latency":12.5,"inference_latency":11.5}`
	tests := map[string]string{
		"missing":            strings.Replace(valid, `,{"index":1,"relevance_score":0.9}`, "", 1),
		"duplicate":          strings.Replace(valid, `"index":1`, `"index":0`, 1),
		"outside":            strings.Replace(valid, `"index":1`, `"index":2`, 1),
		"negative":           strings.Replace(valid, `0.2`, `-0.2`, 1),
		"non finite latency": strings.Replace(valid, `12.5`, `1e1000`, 1),
		"unknown":            strings.Replace(valid, `"total_bytes":`, `"private":true,"total_bytes":`, 1),
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			profile := testProfile(LatencyAuto)
			client := testClient(t, profile, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				require.NoError(t, err)
				assert.NotContains(t, string(body), `"latency"`)
				return rerankJSONResponse(request, http.StatusOK, []byte(response)), nil
			}))
			_, err := client.Rerank(context.Background(), rerankingRequest())
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestRerankRejectsDuplicateStableIDsAndProviderPayloadBeforeSecrets(t *testing.T) {
	profile := testProfile(LatencySlow)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, assert.AnError
	}))
	request := rerankingRequest()
	request.Candidates[1].Document = request.Candidates[0].Document
	_, err := client.Rerank(context.Background(), request)
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Zero(t, secrets.calls.Load())

	request = rerankingRequest()
	request.Query = strings.Repeat("q", profile.MaxQueryBytes)
	request.Candidates[0].Excerpt = strings.Repeat("d", int(profile.MaxTotalExcerptBytes/2))
	request.Candidates[1].Excerpt = strings.Repeat("d", int(profile.MaxTotalExcerptBytes/2))
	_, err = client.Rerank(context.Background(), request)
	require.ErrorIs(t, err, ErrCapacityResponse)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestRerankRejectsIncompleteStableIDsBeforeSecretsOrEgress(t *testing.T) {
	tests := map[string]func(*retrieval.DocumentIdentity){
		"vault":   func(value *retrieval.DocumentIdentity) { value.VaultID = "" },
		"node":    func(value *retrieval.DocumentIdentity) { value.NodeID = 0 },
		"version": func(value *retrieval.DocumentIdentity) { value.ContentVersionID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, testProfile(LatencyFast), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, assert.AnError
			}))
			request := rerankingRequest()
			mutate(&request.Candidates[0].Document)

			_, err := client.Rerank(context.Background(), request)
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestRerankRequiresExplicitLatencyToMatchProviderMode(t *testing.T) {
	profile := testProfile(LatencyFast)
	client := testClient(t, profile, testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := []byte(`{"results":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.9}],"total_bytes":360,"total_tokens":12,"actual_latency_mode":"slow","e2e_latency":12.5,"inference_latency":11.5}`)
		return rerankJSONResponse(request, http.StatusOK, response), nil
	}))

	_, err := client.Rerank(context.Background(), rerankingRequest())
	require.ErrorIs(t, err, ErrPermanentResponse)
}

func TestRerankPreservesCancellationDuringResponseRead(t *testing.T) {
	started := make(chan struct{})
	client := testClient(t, testProfile(LatencyFast), testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: &contextBody{ctx: request.Context()}, Request: request}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Rerank(ctx, rerankingRequest())
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

func rerankingRequest() retrieval.RerankingRequest {
	return retrieval.RerankingRequest{Query: "private query", Candidates: []retrieval.RerankingCandidate{
		{Document: retrieval.DocumentIdentity{VaultID: "vault-private", NodeID: 1, ContentVersionID: "version-private-1"},
			Excerpt: "first excerpt", Evidence: []retrieval.EvidenceReference{{Kind: "rendition_segment", BuildID: "build-private", SegmentID: "segment-private"}}},
		{Document: retrieval.DocumentIdentity{VaultID: "vault-private", NodeID: 2, ContentVersionID: "version-private-2"},
			Excerpt: "second excerpt"},
	}}
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

func rerankJSONResponse(request *http.Request, status int, body []byte) *http.Response {
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
