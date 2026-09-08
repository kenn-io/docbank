package zeroentropyrerank

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/retrieval"
)

const synchronizationWatchdog = 5 * time.Second

func TestRerankBindsPayloadAndScoresToValidatedCandidateSnapshot(t *testing.T) {
	request := rerankingRequest()
	wantDocuments := []retrieval.DocumentIdentity{request.Candidates[0].Document, request.Candidates[1].Document}
	callerCandidates := request.Candidates
	var captured wireRequest
	resolver := mutatingSecretResolver(func() {
		callerCandidates[0] = callerCandidates[1]
		callerCandidates[1] = retrieval.RerankingCandidate{
			Document: retrieval.DocumentIdentity{VaultID: "replacement-vault", NodeID: 99, ContentVersionID: "replacement-version"},
			Excerpt:  "replacement excerpt",
		}
	})
	client := testClient(t, testProfile(LatencyFast), resolver, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(body, &captured, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		return rerankJSONResponse(request, http.StatusOK, []byte(`{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.2}],"total_bytes":360,"total_tokens":12,"actual_latency_mode":"fast","e2e_latency":0.25,"inference_latency":0.2}`)), nil
	}))

	execution, err := client.RerankWithReceipt(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, []string{"first excerpt", "second excerpt"}, captured.Documents)
	assert.Equal(t, []retrieval.RerankScore{
		{Document: wantDocuments[0], Score: 0.2},
		{Document: wantDocuments[1], Score: 0.9},
	}, execution.Scores)
}

func TestRerankEnforcesExactProviderByteCapacityBeforeSecretsOrEgress(t *testing.T) {
	for _, test := range []struct {
		name        string
		extraByte   bool
		wantFailure bool
	}{
		{name: "equal"},
		{name: "one byte over", extraByte: true, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := testProfile(LatencyFast)
			profile.MaxCandidates = 100
			profile.MaxQueryBytes = 1000
			profile.MaxExcerptBytes = 48851
			profile.MaxTotalExcerptBytes = 4_885_001
			profile.MaxRequestBytes = 16 << 20
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, profile, secrets, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				return rerankJSONResponse(request, http.StatusOK, providerCapacityResponse(100)), nil
			}))
			candidates := make([]retrieval.RerankingCandidate, 100)
			for index := range candidates {
				excerptBytes := 48850
				if index == 0 && test.extraByte {
					excerptBytes++
				}
				candidates[index] = retrieval.RerankingCandidate{
					Document: retrieval.DocumentIdentity{VaultID: "synthetic-vault", NodeID: int64(index + 1),
						ContentVersionID: fmt.Sprintf("synthetic-version-%d", index+1)},
					Excerpt: strings.Repeat("x", excerptBytes),
				}
			}

			execution, err := client.RerankWithReceipt(t.Context(), retrieval.RerankingRequest{
				Query: strings.Repeat("q", 1000), Candidates: candidates,
			})
			if test.wantFailure {
				require.ErrorIs(t, err, ErrCapacityResponse)
				assert.Equal(t, Execution{}, execution)
				assert.Zero(t, secrets.calls.Load())
				assert.Zero(t, requests.Load())
				return
			}
			require.NoError(t, err)
			require.Len(t, execution.Scores, 100)
			assert.Equal(t, int32(1), secrets.calls.Load())
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestRerankEnforcesSerializedRequestAndResponseBounds(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		profile := testProfile(LatencyFast)
		profile.MaxRequestBytes = 1
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := testClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, assert.AnError
		}))

		execution, err := client.RerankWithReceipt(t.Context(), rerankingRequest())
		require.ErrorIs(t, err, ErrCapacityResponse)
		assert.Equal(t, Execution{}, execution)
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("response", func(t *testing.T) {
		profile := testProfile(LatencyFast)
		profile.MaxResponseBytes = 1
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := testClient(t, profile, secrets, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			return rerankJSONResponse(request, http.StatusOK, []byte(`{}`)), nil
		}))

		execution, err := client.RerankWithReceipt(t.Context(), rerankingRequest())
		require.ErrorIs(t, err, ErrCapacityResponse)
		assert.Equal(t, Execution{}, execution)
		assert.Equal(t, int32(1), secrets.calls.Load())
		assert.Equal(t, int32(1), requests.Load())
	})
}

func TestRerankRejectsMissingNullAndStructurallyInvalidResponseValues(t *testing.T) {
	valid := `{"results":[{"index":0,"relevance_score":0}],"total_bytes":0,"total_tokens":0,"actual_latency_mode":"fast","e2e_latency":0,"inference_latency":0}`
	tests := []struct {
		name      string
		body      string
		wantValid bool
	}{
		{name: "legitimate zeros", body: valid, wantValid: true},
		{name: "null index", body: strings.Replace(valid, `"index":0`, `"index":null`, 1)},
		{name: "missing index", body: strings.Replace(valid, `"index":0,`, "", 1)},
		{name: "null score", body: strings.Replace(valid, `"relevance_score":0`, `"relevance_score":null`, 1)},
		{name: "missing score", body: strings.Replace(valid, `,"relevance_score":0`, "", 1)},
		{name: "score above one", body: strings.Replace(valid, `"relevance_score":0`, `"relevance_score":1.1`, 1)},
		{name: "nonfinite score", body: strings.Replace(valid, `"relevance_score":0`, `"relevance_score":1e1000`, 1)},
		{name: "null total bytes", body: strings.Replace(valid, `"total_bytes":0`, `"total_bytes":null`, 1)},
		{name: "missing total bytes", body: strings.Replace(valid, `,"total_bytes":0`, "", 1)},
		{name: "negative total bytes", body: strings.Replace(valid, `"total_bytes":0`, `"total_bytes":-1`, 1)},
		{name: "excessive total bytes", body: strings.Replace(valid, `"total_bytes":0`, `"total_bytes":1125899906842625`, 1)},
		{name: "null total tokens", body: strings.Replace(valid, `"total_tokens":0`, `"total_tokens":null`, 1)},
		{name: "missing total tokens", body: strings.Replace(valid, `,"total_tokens":0`, "", 1)},
		{name: "negative total tokens", body: strings.Replace(valid, `"total_tokens":0`, `"total_tokens":-1`, 1)},
		{name: "excessive total tokens", body: strings.Replace(valid, `"total_tokens":0`, `"total_tokens":1125899906842625`, 1)},
		{name: "null actual latency", body: strings.Replace(valid, `"actual_latency_mode":"fast"`, `"actual_latency_mode":null`, 1)},
		{name: "missing actual latency", body: strings.Replace(valid, `,"actual_latency_mode":"fast"`, "", 1)},
		{name: "invalid actual latency", body: strings.Replace(valid, `"actual_latency_mode":"fast"`, `"actual_latency_mode":"instant"`, 1)},
		{name: "null end to end latency", body: strings.Replace(valid, `"e2e_latency":0`, `"e2e_latency":null`, 1)},
		{name: "missing end to end latency", body: strings.Replace(valid, `,"e2e_latency":0`, "", 1)},
		{name: "negative end to end latency", body: strings.Replace(valid, `"e2e_latency":0`, `"e2e_latency":-1`, 1)},
		{name: "excessive end to end latency", body: strings.Replace(valid, `"e2e_latency":0`, `"e2e_latency":86401`, 1)},
		{name: "null inference latency", body: strings.Replace(valid, `"inference_latency":0`, `"inference_latency":null`, 1)},
		{name: "missing inference latency", body: strings.Replace(valid, `,"inference_latency":0`, "", 1)},
		{name: "negative inference latency", body: strings.Replace(valid, `"inference_latency":0`, `"inference_latency":-1`, 1)},
		{name: "excessive inference latency", body: strings.Replace(valid, `"inference_latency":0`, `"inference_latency":86401`, 1)},
		{name: "inference exceeds end to end", body: strings.Replace(valid, `"inference_latency":0`, `"inference_latency":0.1`, 1)},
		{name: "duplicate member", body: strings.Replace(valid, `"total_bytes":0`, `"total_bytes":0,"total_bytes":0`, 1)},
		{name: "trailing JSON", body: valid + `{}`},
		{name: "unknown member", body: strings.Replace(valid, `"total_bytes":0`, `"synthetic_provider_detail":"redacted","total_bytes":0`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, testProfile(LatencyFast), testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return rerankJSONResponse(request, http.StatusOK, []byte(test.body)), nil
			}))
			request := retrieval.RerankingRequest{Query: "synthetic query", Candidates: []retrieval.RerankingCandidate{{
				Document: retrieval.DocumentIdentity{VaultID: "synthetic-vault", NodeID: 1, ContentVersionID: "synthetic-version"},
				Excerpt:  "synthetic excerpt",
			}}}

			execution, err := client.RerankWithReceipt(t.Context(), request)
			if test.wantValid {
				require.NoError(t, err)
				require.Len(t, execution.Scores, 1)
				assert.Zero(t, execution.Scores[0].Score)
				assert.Zero(t, execution.Receipt.TotalBytes)
				assert.Zero(t, execution.Receipt.TotalTokens)
				assert.Zero(t, execution.Receipt.E2ELatencySeconds)
				assert.Zero(t, execution.Receipt.InferenceLatencySeconds)
				return
			}
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, Execution{}, execution)
			assert.NotContains(t, err.Error(), "synthetic_provider_detail")
		})
	}
}

func TestRerankWithReceiptKeepsConcurrentExecutionsRequestLocalAndRedacted(t *testing.T) {
	type responseSpec struct {
		candidateCount int
		totalBytes     int64
		totalTokens    int64
		actualLatency  Latency
		e2eLatency     float64
		inference      float64
	}
	specs := map[string]responseSpec{
		"synthetic query one": {candidateCount: 2, totalBytes: 201, totalTokens: 21, actualLatency: LatencyFast, e2eLatency: 0.25, inference: 0.2},
		"synthetic query two": {candidateCount: 3, totalBytes: 302, totalTokens: 32, actualLatency: LatencyFast, e2eLatency: 0.5, inference: 0.4},
	}
	arrived := make(chan struct{}, len(specs))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	client := testClient(t, testProfile(LatencyFast), testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var payload wireRequest
		if err := json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		spec, ok := specs[payload.Query]
		if !ok || len(payload.Documents) != spec.candidateCount {
			return nil, errors.New("unexpected synthetic request")
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return rerankJSONResponse(request, http.StatusOK, concurrentResponse(spec)), nil
	}))

	requests := []retrieval.RerankingRequest{
		concurrentRequest("one", "synthetic query one", 2),
		concurrentRequest("two", "synthetic query two", 3),
	}
	type callResult struct {
		index     int
		execution Execution
		err       error
	}
	results := make(chan callResult, len(requests))
	testContext := t.Context()
	for index, request := range requests {
		go func(index int, request retrieval.RerankingRequest) {
			execution, err := client.RerankWithReceipt(testContext, request)
			results <- callResult{index: index, execution: execution, err: err}
		}(index, request)
	}
	for range requests {
		select {
		case <-arrived:
		case <-time.After(synchronizationWatchdog):
			require.FailNow(t, "timed out waiting for concurrent rerank calls")
		}
	}
	releaseAll()

	got := make([]Execution, len(requests))
	for range requests {
		select {
		case result := <-results:
			require.NoError(t, result.err)
			got[result.index] = result.execution
		case <-time.After(synchronizationWatchdog):
			require.FailNow(t, "timed out waiting for concurrent rerank results")
		}
	}
	for index, request := range requests {
		spec := specs[request.Query]
		require.Len(t, got[index].Scores, len(request.Candidates))
		for candidateIndex := range request.Candidates {
			assert.Equal(t, request.Candidates[candidateIndex].Document, got[index].Scores[candidateIndex].Document)
			assert.InDelta(t, float64(candidateIndex)/10, got[index].Scores[candidateIndex].Score, 0)
		}
		assert.Equal(t, len(request.Candidates), got[index].Receipt.CandidateCount)
		assert.Equal(t, spec.totalBytes, got[index].Receipt.TotalBytes)
		assert.Equal(t, spec.totalTokens, got[index].Receipt.TotalTokens)
		assert.Equal(t, spec.actualLatency, got[index].Receipt.ActualLatency)
		assert.InDelta(t, spec.e2eLatency, got[index].Receipt.E2ELatencySeconds, 0)
		assert.InDelta(t, spec.inference, got[index].Receipt.InferenceLatencySeconds, 0)
		receiptText := fmt.Sprintf("%+v", got[index].Receipt)
		assert.NotContains(t, receiptText, request.Query)
		for _, candidate := range request.Candidates {
			assert.NotContains(t, receiptText, candidate.Excerpt)
			for _, evidence := range candidate.Evidence {
				assert.NotContains(t, receiptText, evidence.Kind)
			}
		}
	}
}

func TestRerankCanceledBeforeWorkDoesNotResolveSecretsOrEgress(t *testing.T) {
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, testProfile(LatencyFast), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, assert.AnError
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	execution, err := client.RerankWithReceipt(ctx, rerankingRequest())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Execution{}, execution)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestRerankRejectsLateSuccessAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testClient(t, testProfile(LatencyFast), testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := []byte(`{"results":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.9}],"total_bytes":360,"total_tokens":12,"actual_latency_mode":"fast","e2e_latency":0.25,"inference_latency":0.2}`)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: &cancelOnEOFBody{reader: bytes.NewReader(body), cancel: cancel}, Request: request}, nil
	}))

	execution, err := client.RerankWithReceipt(ctx, rerankingRequest())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Execution{}, execution)
}

func TestRerankClassifiesHTTPFailuresWithoutProviderContent(t *testing.T) {
	const providerContent = "sensitive synthetic provider detail"
	for _, test := range []struct {
		name      string
		status    int
		kind      error
		wantRetry bool
	}{
		{name: "transient", status: http.StatusTooManyRequests, kind: ErrTransientResponse, wantRetry: true},
		{name: "capacity", status: http.StatusRequestEntityTooLarge, kind: ErrCapacityResponse},
		{name: "permanent", status: http.StatusBadRequest, kind: ErrPermanentResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, testProfile(LatencyFast), testSecrets{"secret:zeroentropy-rerank": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := rerankJSONResponse(request, test.status, []byte(providerContent))
				response.Header.Set("Retry-After", "7200")
				return response, nil
			}))

			execution, err := client.RerankWithReceipt(t.Context(), rerankingRequest())
			require.ErrorIs(t, err, test.kind)
			assert.Equal(t, Execution{}, execution)
			assert.NotContains(t, err.Error(), providerContent)
			delay, set := RetryAfter(err)
			assert.Equal(t, test.wantRetry, set)
			if test.wantRetry {
				assert.Equal(t, time.Hour, delay)
			} else {
				assert.Zero(t, delay)
			}
		})
	}
}

func providerCapacityResponse(count int) []byte {
	results := make([]wireResult, count)
	for index := range results {
		current := index
		score := float64(index) / float64(count)
		results[index] = wireResult{Index: &current, RelevanceScore: &score}
	}
	totalBytes, totalTokens := int64(0), int64(0)
	e2e, inference := 0.0, 0.0
	body, _ := json.Marshal(wireResponse{Results: results, TotalBytes: &totalBytes, TotalTokens: &totalTokens,
		ActualLatencyMode: LatencyFast, E2ELatency: &e2e, InferenceLatency: &inference})
	return body
}

func concurrentResponse(spec struct {
	candidateCount int
	totalBytes     int64
	totalTokens    int64
	actualLatency  Latency
	e2eLatency     float64
	inference      float64
}) []byte {
	results := make([]wireResult, spec.candidateCount)
	for position := range results {
		index := spec.candidateCount - position - 1
		score := float64(index) / 10
		results[position] = wireResult{Index: &index, RelevanceScore: &score}
	}
	body, _ := json.Marshal(wireResponse{Results: results, TotalBytes: &spec.totalBytes, TotalTokens: &spec.totalTokens,
		ActualLatencyMode: spec.actualLatency, E2ELatency: &spec.e2eLatency, InferenceLatency: &spec.inference})
	return body
}

func concurrentRequest(label, query string, count int) retrieval.RerankingRequest {
	candidates := make([]retrieval.RerankingCandidate, count)
	for index := range candidates {
		candidates[index] = retrieval.RerankingCandidate{
			Document: retrieval.DocumentIdentity{VaultID: "synthetic-vault-" + label, NodeID: int64(index + 1),
				ContentVersionID: fmt.Sprintf("synthetic-version-%s-%d", label, index+1)},
			Excerpt:  fmt.Sprintf("synthetic excerpt %s %d", label, index+1),
			Evidence: []retrieval.EvidenceReference{{Kind: "synthetic-evidence-" + label}},
		}
	}
	return retrieval.RerankingRequest{Query: query, Candidates: candidates}
}

type mutatingSecretResolver func()

func (resolver mutatingSecretResolver) ResolveSecret(context.Context, string) (string, error) {
	resolver()
	return "synthetic-key", nil
}

type cancelOnEOFBody struct {
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (body *cancelOnEOFBody) Read(buffer []byte) (int, error) {
	count, err := body.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		body.cancel()
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read synthetic late-success body: %w", err)
	}
	return count, nil
}

func (*cancelOnEOFBody) Close() error { return nil }
