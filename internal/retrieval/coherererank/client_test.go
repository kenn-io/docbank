package coherererank

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/retrieval"
)

func TestRerankSendsOnlyBoundedTextAndMapsIndicesLocally(t *testing.T) {
	profile := testProfile(ModelPro)
	client := testClient(t, profile, testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "https://api.cohere.com/v2/rerank", request.URL.String())
		assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		for _, localOnly := range []string{"vault-local", "version-local", "build-local", "segment-local"} {
			assert.NotContains(t, string(body), localOnly)
		}
		var payload struct {
			Model           Model    `json:"model"`
			Query           string   `json:"query"`
			Documents       []string `json:"documents"`
			TopN            int      `json:"top_n"`
			MaxTokensPerDoc int      `json:"max_tokens_per_doc"`
		}
		require.NoError(t, json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)))
		assert.Equal(t, ModelPro, payload.Model)
		assert.Equal(t, "synthetic query", payload.Query)
		assert.Equal(t, []string{"first excerpt", "second excerpt"}, payload.Documents)
		assert.Equal(t, 2, payload.TopN)
		assert.Equal(t, 4096, payload.MaxTokensPerDoc)
		response := []byte(`{"id":"response-id","results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.2}],"meta":{"api_version":{"version":"2","is_deprecated":false,"is_experimental":false},"billed_units":{"search_units":1},"tokens":{"input_tokens":7}}}`)
		return jsonResponse(request, http.StatusOK, response), nil
	}))
	request := rerankingRequest()

	execution, err := client.RerankWithReceipt(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, []retrieval.RerankScore{
		{Document: request.Candidates[0].Document, Score: 0.2},
		{Document: request.Candidates[1].Document, Score: 0.9},
	}, execution.Scores)
	assert.Equal(t, Receipt{PolicyFingerprint: client.PolicyFingerprint(),
		Model: ModelPro, ModelRevision: profile.ModelRevision, CandidateCount: 2,
		InputTokens: 7, SearchUnits: 1, ProviderResponseID: "response-id"}, execution.Receipt)
}

func TestRerankSnapshotsCandidatesBeforeSecretCallback(t *testing.T) {
	request := rerankingRequest()
	original := []retrieval.RerankingCandidate{request.Candidates[0], request.Candidates[1]}
	resolver := secretResolverFunc(func(context.Context, string) (string, error) {
		request.Candidates[0] = original[1]
		request.Candidates[1] = retrieval.RerankingCandidate{
			Document: retrieval.DocumentIdentity{VaultID: "replacement", NodeID: 99, ContentVersionID: "replacement"},
			Excerpt:  "replacement excerpt",
		}
		return "synthetic-key", nil
	})
	client := testClient(t, testProfile(ModelPro), resolver, roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		var payload wireRequest
		require.NoError(t, json.UnmarshalRead(httpRequest.Body, &payload, json.RejectUnknownMembers(true)))
		assert.Equal(t, []string{original[0].Excerpt, original[1].Excerpt}, payload.Documents)
		return jsonResponse(httpRequest, http.StatusOK, []byte(`{"results":[{"index":1,"relevance_score":0.8},{"index":0,"relevance_score":0.3}]}`)), nil
	}))

	execution, err := client.RerankWithReceipt(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, []retrieval.RerankScore{
		{Document: original[0].Document, Score: 0.3},
		{Document: original[1].Document, Score: 0.8},
	}, execution.Scores)
}

func TestRerankRejectsIncompleteOrDuplicateIdentityBeforeSecretOrEgress(t *testing.T) {
	tests := map[string]func(*retrieval.RerankingRequest){
		"missing vault": func(request *retrieval.RerankingRequest) { request.Candidates[0].Document.VaultID = "" },
		"zero node":     func(request *retrieval.RerankingRequest) { request.Candidates[0].Document.NodeID = 0 },
		"negative node": func(request *retrieval.RerankingRequest) { request.Candidates[0].Document.NodeID = -1 },
		"missing version": func(request *retrieval.RerankingRequest) {
			request.Candidates[0].Document.ContentVersionID = ""
		},
		"duplicate identity": func(request *retrieval.RerankingRequest) {
			request.Candidates[1].Document = request.Candidates[0].Document
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, testProfile(ModelPro), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request must not run")
			}))
			request := rerankingRequest()
			mutate(&request)

			execution, err := client.RerankWithReceipt(context.Background(), request)
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, Execution{}, execution)
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestRerankRejectsMissingOrNullScoreFieldsWithZeroExecution(t *testing.T) {
	bodies := []string{
		`{"results":[{"index":0,"relevance_score":null},{"index":1,"relevance_score":0.4}]}`,
		`{"results":[{"relevance_score":0.5},{"index":1,"relevance_score":0.4}]}`,
		`{"results":[{"index":null,"relevance_score":0.5},{"index":1,"relevance_score":0.4}]}`,
	}
	for _, body := range bodies {
		client := testClient(t, testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return jsonResponse(request, http.StatusOK, []byte(body)), nil
		}))
		execution, err := client.RerankWithReceipt(context.Background(), rerankingRequest())
		require.ErrorIs(t, err, ErrPermanentResponse)
		assert.Equal(t, Execution{}, execution)
		assert.NotContains(t, err.Error(), body)
	}
}

func TestRerankAcceptsNumericZeroIndexAndScore(t *testing.T) {
	client := testClient(t, testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, []byte(`{"results":[{"index":0,"relevance_score":0},{"index":1,"relevance_score":0.4}]}`)), nil
	}))
	request := rerankingRequest()

	execution, err := client.RerankWithReceipt(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, []retrieval.RerankScore{
		{Document: request.Candidates[0].Document, Score: 0},
		{Document: request.Candidates[1].Document, Score: 0.4},
	}, execution.Scores)
}

func TestRerankRejectsInconsistentProviderUsage(t *testing.T) {
	client := testClient(t, testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := []byte(`{"id":"response-id","results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"input_tokens":7},"tokens":{"input_tokens":8}}}`)
		return jsonResponse(request, http.StatusOK, body), nil
	}))

	_, err := client.Rerank(context.Background(), rerankingRequest())
	require.ErrorIs(t, err, ErrPermanentResponse)
}

func TestRerankAcceptsDocumentedFractionalUsageAndPreservesItInReceipt(t *testing.T) {
	client := testClient(t, testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := []byte(`{"id":"response-id","results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"images":0.0,"input_tokens":1.5,"image_tokens":0.0,"output_tokens":3.5,"search_units":4.25,"classifications":5.5,"pages":6.75},"tokens":{"input_tokens":1.5,"output_tokens":3.5},"cached_tokens":0.75}}`)
		return jsonResponse(request, http.StatusOK, body), nil
	}))

	execution, err := client.RerankWithReceipt(context.Background(), rerankingRequest())
	require.NoError(t, err)
	assert.Zero(t, execution.Receipt.BilledImages)
	assert.InDelta(t, 1.5, execution.Receipt.InputTokens, 0)
	assert.Zero(t, execution.Receipt.ImageTokens)
	assert.InDelta(t, 3.5, execution.Receipt.OutputTokens, 0)
	assert.InDelta(t, 4.25, execution.Receipt.SearchUnits, 0)
	assert.InDelta(t, 5.5, execution.Receipt.Classifications, 0)
	assert.InDelta(t, 6.75, execution.Receipt.Pages, 0)
	assert.InDelta(t, 0.75, execution.Receipt.CachedTokens, 0)
}

func TestRerankRejectsNonzeroImageUsageForTextOnlyRequest(t *testing.T) {
	for name, body := range map[string]string{
		"images":       `{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"images":0.5}}}`,
		"image tokens": `{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"image_tokens":2.25}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(body)), nil
			}))
			_, err := client.Rerank(context.Background(), rerankingRequest())
			require.ErrorIs(t, err, ErrPermanentResponse)
		})
	}
}

func TestRerankAcceptsDocumentedResponseWithoutOptionalID(t *testing.T) {
	client := testClient(t, testProfile(ModelFast), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := []byte(`{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}]}`)
		return jsonResponse(request, http.StatusOK, body), nil
	}))

	execution, err := client.RerankWithReceipt(context.Background(), rerankingRequest())
	require.NoError(t, err)
	assert.Empty(t, execution.Receipt.ProviderResponseID)
	require.Len(t, execution.Scores, 2)
}

func TestRerankRejectsBlankQueryBeforeSecretOrEgress(t *testing.T) {
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, testProfile(ModelPro), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))
	request := rerankingRequest()
	request.Query = " \t\n"

	_, err := client.Rerank(context.Background(), request)
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestRerankRejectsInvalidOrOversizedTextBeforeSecretOrEgress(t *testing.T) {
	tests := map[string]struct {
		configure func(*Profile)
		mutate    func(*retrieval.RerankingRequest)
		want      error
	}{
		"invalid query UTF-8": {
			mutate: func(request *retrieval.RerankingRequest) { request.Query = string([]byte{0xff}) },
			want:   ErrPermanentResponse,
		},
		"oversized query": {
			mutate: func(request *retrieval.RerankingRequest) { request.Query = string(make([]byte, 4097)) },
			want:   ErrCapacityResponse,
		},
		"invalid excerpt UTF-8": {
			mutate: func(request *retrieval.RerankingRequest) { request.Candidates[0].Excerpt = string([]byte{0xff}) },
			want:   ErrCapacityResponse,
		},
		"oversized excerpt": {
			mutate: func(request *retrieval.RerankingRequest) { request.Candidates[0].Excerpt = string(make([]byte, 4097)) },
			want:   ErrCapacityResponse,
		},
		"oversized aggregate": {
			configure: func(profile *Profile) { profile.MaxTotalExcerptBytes = 4096 },
			mutate: func(request *retrieval.RerankingRequest) {
				request.Candidates[0].Excerpt = string(make([]byte, 2049))
				request.Candidates[1].Excerpt = string(make([]byte, 2049))
			},
			want: ErrCapacityResponse,
		},
		"too many candidates": {
			configure: func(profile *Profile) { profile.MaxCandidates = 1 },
			mutate:    func(*retrieval.RerankingRequest) {},
			want:      ErrCapacityResponse,
		},
		"oversized encoded request": {
			configure: func(profile *Profile) { profile.MaxRequestBytes = 1 },
			mutate:    func(*retrieval.RerankingRequest) {},
			want:      ErrCapacityResponse,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			profile := testProfile(ModelPro)
			if test.configure != nil {
				test.configure(&profile)
			}
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request must not run")
			}))
			request := rerankingRequest()
			test.mutate(&request)

			execution, err := client.RerankWithReceipt(context.Background(), request)
			require.ErrorIs(t, err, test.want)
			assert.Equal(t, Execution{}, execution)
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestRerankRejectsStrictResponseDriftAndInvalidScores(t *testing.T) {
	tests := map[string]string{
		"missing":        `{"id":"response","results":[{"index":0,"relevance_score":0.2}]}`,
		"duplicate":      `{"id":"response","results":[{"index":0,"relevance_score":0.2},{"index":0,"relevance_score":0.3}]}`,
		"outside":        `{"id":"response","results":[{"index":0,"relevance_score":0.2},{"index":2,"relevance_score":0.3}]}`,
		"negative":       `{"id":"response","results":[{"index":0,"relevance_score":-0.1},{"index":1,"relevance_score":0.3}]}`,
		"above one":      `{"id":"response","results":[{"index":0,"relevance_score":1.1},{"index":1,"relevance_score":0.3}]}`,
		"non-finite":     `{"id":"response","results":[{"index":0,"relevance_score":1e999},{"index":1,"relevance_score":0.3}]}`,
		"unknown":        `{"id":"response","results":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.3}],"unexpected":"body"}`,
		"usage overflow": `{"id":"response","results":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.3}],"meta":{"billed_units":{"search_units":` + strconv.FormatInt(math.MaxInt64, 10) + `}}}`,
		"unsafe id":      `{"id":"unsafe response id","results":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.3}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, testProfile(ModelFast), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(body)), nil
			}))
			_, err := client.Rerank(context.Background(), rerankingRequest())
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.NotContains(t, err.Error(), "unexpected")
			assert.NotContains(t, err.Error(), "unsafe")
		})
	}
}

func TestRerankClassifiesSanitizedHTTPFailures(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusRequestTimeout, ErrTransientResponse},
		{http.StatusTooManyRequests, ErrTransientResponse},
		{http.StatusInternalServerError, ErrTransientResponse},
		{http.StatusRequestEntityTooLarge, ErrCapacityResponse},
		{http.StatusBadRequest, ErrPermanentResponse},
		{http.StatusTemporaryRedirect, ErrPermanentResponse},
	}
	for _, test := range tests {
		client := testClient(t, testProfile(ModelPro), testSecrets{"secret:cohere-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := jsonResponse(request, test.status, []byte(`{"unexpected":"provider body"}`))
			response.Header.Set("Retry-After", "7200")
			return response, nil
		}))
		_, err := client.Rerank(context.Background(), rerankingRequest())
		require.ErrorIs(t, err, test.want)
		assert.NotContains(t, err.Error(), "provider body")
		if test.status == http.StatusTooManyRequests {
			delay, ok := RetryAfter(err)
			assert.True(t, ok)
			assert.Equal(t, time.Hour, delay)
		}
	}
}

func rerankingRequest() retrieval.RerankingRequest {
	return retrieval.RerankingRequest{Query: "synthetic query", Candidates: []retrieval.RerankingCandidate{
		{Document: retrieval.DocumentIdentity{VaultID: "vault-local", NodeID: 1, ContentVersionID: "version-local-1"},
			Excerpt: "first excerpt", Evidence: []retrieval.EvidenceReference{{Kind: "rendition_segment", BuildID: "build-local", SegmentID: "segment-local"}}},
		{Document: retrieval.DocumentIdentity{VaultID: "vault-local", NodeID: 2, ContentVersionID: "version-local-2"},
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

func jsonResponse(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: request}
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
