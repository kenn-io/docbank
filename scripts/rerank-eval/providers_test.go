package rerankeval_test

import (
	"context"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/document/typesafe"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/retrieval/cohere"
)

func TestProviderMapping(t *testing.T) {
	candidates := []embeddingeval.Document{{ID: "judgment-grade-candidate-a.txt", Text: "first synthetic excerpt"}, {ID: "judgment-grade-candidate-b.txt", Text: "second synthetic excerpt"}}
	for _, shape := range []typesafe.RequestShape{typesafe.RequestShapePerCandidate, typesafe.RequestShapeBatched} {
		t.Run(string(shape), func(t *testing.T) {
			fake := &fakeTypeSafeClient{shape: shape, fingerprint: "jev-policy", result: typesafe.Result{
				Scores: []float64{0.2, 0.8}, Receipt: typesafe.Receipt{PolicyFingerprint: "jev-policy", RequestShape: shape, CandidateCount: 2, InputTokens: 4, OutputTokens: 2},
			}}
			adapter := &typeSafeAdapter{client: fake, pricing: &pricingInput{TypeSafeTokens: &datedRate{Basis: "2026-09-21:typesafe-token", MicrosPerUnit: 2.5}}}
			result, err := adapter.Rerank(context.Background(), embeddingeval.System{RerankerFingerprint: "jev-policy"}, "synthetic query", candidates)
			require.NoError(t, err)
			assert.Equal(t, []float64{0.2, 0.8}, result.Scores)
			wantCalls := 1
			if shape == typesafe.RequestShapePerCandidate {
				wantCalls = len(candidates)
			}
			assert.Equal(t, wantCalls, result.Usage.ProviderCalls)
			require.NotNil(t, result.Usage.TokenUsage)
			assert.InDelta(t, 6.0, *result.Usage.TokenUsage, 0.0001)
			require.NotNil(t, result.Usage.Cost)
			assert.Equal(t, int64(15), result.Usage.Cost.Micros)
			assert.Equal(t, "2026-09-21:typesafe-token", result.Usage.Cost.Basis)
			assert.Equal(t, shape, fake.requests[0].Shape)
			for _, request := range fake.requests {
				assert.Equal(t, "synthetic query", request.Query)
				assert.Equal(t, []string{"first synthetic excerpt", "second synthetic excerpt"}, request.Candidates)
				assert.NotContains(t, strings.Join(request.Candidates, " "), "judgment-grade-candidate")
			}
		})
	}

	cohereFake := &fakeCohereClient{execution: cohere.Execution{
		Scores: []retrieval.RerankScore{
			{Document: retrieval.DocumentIdentity{VaultID: "synthetic", NodeID: 2, ContentVersionID: "judgment-grade-candidate-b.txt"}, Score: 0.75},
			{Document: retrieval.DocumentIdentity{VaultID: "synthetic", NodeID: 1, ContentVersionID: "judgment-grade-candidate-a.txt"}, Score: 0.25},
		},
		Receipt: cohere.Receipt{InputTokens: 1.5, OutputTokens: 2.25, SearchUnits: 4.5},
	}}
	cohereAdapter := &cohereProviderAdapter{client: cohereFake, pricing: &pricingInput{CohereSearchUnits: &datedRate{Basis: "2026-09-21:cohere-search-unit", MicrosPerUnit: 2}}}
	result, err := cohereAdapter.Rerank(context.Background(), embeddingeval.System{RerankerFingerprint: "cohere-policy"}, "synthetic query", candidates)
	require.NoError(t, err)
	assert.Equal(t, []float64{0.25, 0.75}, result.Scores)
	assert.Equal(t, 1, result.Usage.ProviderCalls)
	assert.Nil(t, result.Usage.TokenUsage, "Cohere receipt has no token-presence bit")
	require.NotNil(t, result.Usage.Cost)
	assert.Equal(t, int64(9), result.Usage.Cost.Micros)
	assert.Equal(t, "2026-09-21:cohere-search-unit", result.Usage.Cost.Basis)
	require.Len(t, cohereFake.requests, 1)
	assert.Equal(t, "synthetic query", cohereFake.requests[0].Query)
	assert.Equal(t, []string{"first synthetic excerpt", "second synthetic excerpt"}, []string{
		cohereFake.requests[0].Candidates[0].Excerpt, cohereFake.requests[0].Candidates[1].Excerpt,
	})
	assert.NotContains(t, cohereFake.requests[0].Candidates[0].Excerpt, "judgment-grade-candidate")
	assert.NotContains(t, cohereFake.requests[0].Candidates[1].Excerpt, "judgment-grade-candidate")
	zeroReceipt := &fakeCohereClient{execution: cohere.Execution{
		Scores:  cohereFake.execution.Scores,
		Receipt: cohere.Receipt{},
	}}
	zeroResult, err := (&cohereProviderAdapter{client: zeroReceipt, pricing: &pricingInput{
		CohereSearchUnits: &datedRate{Basis: "2026-09-21:cohere-search-unit", MicrosPerUnit: 2},
	}}).Rerank(context.Background(), embeddingeval.System{}, "synthetic query", candidates)
	require.NoError(t, err)
	assert.Nil(t, zeroResult.Usage.Cost, "Cohere zero search units have no presence bit")
}

type typeSafeAPI interface {
	Rerank(ctx context.Context, request typesafe.RerankRequest) (typesafe.Result, error)
	RequestShape() typesafe.RequestShape
	PolicyFingerprint() string
}

type typeSafeAdapter struct {
	client  typeSafeAPI
	pricing *pricingInput
}

func (adapter *typeSafeAdapter) Rerank(ctx context.Context, _ embeddingeval.System, query string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error) {
	if adapter == nil || adapter.client == nil {
		return embeddingeval.RerankResult{}, errors.New("typesafe adapter client is required")
	}
	texts := make([]string, len(candidates))
	var candidateRunes int
	for index, candidate := range candidates {
		texts[index] = candidate.Text
		candidateRunes += len([]rune(candidate.Text))
	}
	started := time.Now()
	result, err := adapter.client.Rerank(ctx, typesafe.RerankRequest{Query: query, Candidates: texts})
	if err != nil {
		return embeddingeval.RerankResult{}, err
	}
	if result.Receipt.PolicyFingerprint != "" && result.Receipt.PolicyFingerprint != adapter.client.PolicyFingerprint() {
		return embeddingeval.RerankResult{}, errors.New("typesafe adapter returned a mismatched policy fingerprint")
	}
	if result.Receipt.RequestShape != "" && result.Receipt.RequestShape != adapter.client.RequestShape() {
		return embeddingeval.RerankResult{}, errors.New("typesafe adapter returned a mismatched request shape")
	}
	if result.Receipt.CandidateCount != 0 && result.Receipt.CandidateCount != len(candidates) {
		return embeddingeval.RerankResult{}, errors.New("typesafe adapter returned a mismatched candidate count")
	}
	tokens := float64(result.Receipt.InputTokens + result.Receipt.OutputTokens)
	calls := 1
	inputRunes := len([]rune(query)) + candidateRunes
	if adapter.client.RequestShape() == typesafe.RequestShapePerCandidate {
		calls = len(candidates)
		inputRunes += len([]rune(query)) * (len(candidates) - 1)
	}
	cost, err := adapter.pricing.typeSafeCost(tokens)
	if err != nil {
		return embeddingeval.RerankResult{}, err
	}
	return embeddingeval.RerankResult{Scores: slices.Clone(result.Scores), Usage: embeddingeval.Usage{
		ProviderCalls: calls, ProviderInputRunes: inputRunes,
		ProviderOutputUnits: len(result.Scores), Latency: time.Since(started), TokenUsage: &tokens,
		EstimatedCostMicros: costMicros(cost), Cost: cost,
	}}, nil
}

type cohereAPI interface {
	RerankWithReceipt(ctx context.Context, request retrieval.RerankingRequest) (cohere.Execution, error)
	PolicyFingerprint() string
}

type cohereProviderAdapter struct {
	client  cohereAPI
	pricing *pricingInput
}

func (adapter *cohereProviderAdapter) Rerank(ctx context.Context, _ embeddingeval.System, query string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error) {
	if adapter == nil || adapter.client == nil {
		return embeddingeval.RerankResult{}, errors.New("cohere adapter client is required")
	}
	request := retrieval.RerankingRequest{Query: query, Candidates: make([]retrieval.RerankingCandidate, len(candidates))}
	inputRunes := len([]rune(query))
	for index, candidate := range candidates {
		request.Candidates[index] = retrieval.RerankingCandidate{
			Document: retrieval.DocumentIdentity{VaultID: "synthetic", NodeID: int64(index + 1), ContentVersionID: candidate.ID},
			Excerpt:  candidate.Text,
		}
		inputRunes += len([]rune(candidate.Text))
	}
	started := time.Now()
	execution, err := adapter.client.RerankWithReceipt(ctx, request)
	if err != nil {
		return embeddingeval.RerankResult{}, err
	}
	if execution.Receipt.PolicyFingerprint != "" && execution.Receipt.PolicyFingerprint != adapter.client.PolicyFingerprint() {
		return embeddingeval.RerankResult{}, errors.New("cohere adapter returned a mismatched policy fingerprint")
	}
	byDocument := make(map[retrieval.DocumentIdentity]float64, len(execution.Scores))
	for _, score := range execution.Scores {
		byDocument[score.Document] = score.Score
	}
	if len(byDocument) != len(candidates) {
		return embeddingeval.RerankResult{}, errors.New("cohere adapter returned duplicate candidates")
	}
	scores := make([]float64, len(candidates))
	for index, candidate := range request.Candidates {
		score, ok := byDocument[candidate.Document]
		if !ok {
			return embeddingeval.RerankResult{}, errors.New("cohere adapter returned an unknown candidate")
		}
		scores[index] = score
	}
	var cost *embeddingeval.CostObservation
	if execution.Receipt.SearchUnits > 0 {
		cost, err = adapter.pricing.cohereCost(execution.Receipt.SearchUnits)
		if err != nil {
			return embeddingeval.RerankResult{}, err
		}
	}
	return embeddingeval.RerankResult{Scores: scores, Usage: embeddingeval.Usage{
		ProviderCalls: 1, ProviderInputRunes: inputRunes, ProviderOutputUnits: len(scores),
		Latency: time.Since(started), EstimatedCostMicros: costMicros(cost), Cost: cost,
	}}, nil
}

func (adapter *typeSafeAdapter) PolicyFingerprint() string {
	if adapter == nil || adapter.client == nil {
		return ""
	}
	return adapter.client.PolicyFingerprint()
}

func (adapter *cohereProviderAdapter) PolicyFingerprint() string {
	if adapter == nil || adapter.client == nil {
		return ""
	}
	return adapter.client.PolicyFingerprint()
}

type datedRate struct {
	Basis         string
	MicrosPerUnit float64
}

type pricingInput struct {
	TypeSafeTokens    *datedRate
	CohereSearchUnits *datedRate
}

func (pricing *pricingInput) typeSafeCost(tokens float64) (*embeddingeval.CostObservation, error) {
	if pricing == nil {
		return nil, nil //nolint:nilnil // missing pricing deliberately leaves cost unavailable
	}
	return pricing.TypeSafeTokens.cost(tokens)
}

func (pricing *pricingInput) cohereCost(units float64) (*embeddingeval.CostObservation, error) {
	if pricing == nil {
		return nil, nil //nolint:nilnil // missing pricing deliberately leaves cost unavailable
	}
	return pricing.CohereSearchUnits.cost(units)
}

func (rate *datedRate) cost(units float64) (*embeddingeval.CostObservation, error) {
	if rate == nil {
		return nil, nil //nolint:nilnil // missing rate deliberately leaves cost unavailable
	}
	if strings.TrimSpace(rate.Basis) == "" || math.IsNaN(rate.MicrosPerUnit) || math.IsInf(rate.MicrosPerUnit, 0) || rate.MicrosPerUnit < 0 {
		return nil, errors.New("pricing rate must have a finite non-negative value and dated basis")
	}
	if math.IsNaN(units) || math.IsInf(units, 0) || units < 0 {
		return nil, errors.New("provider usage quantity is invalid")
	}
	value := units * rate.MicrosPerUnit
	if math.IsInf(value, 0) || value > float64(^uint64(0)>>1) {
		return nil, errors.New("pricing result is too large")
	}
	return &embeddingeval.CostObservation{Micros: int64(math.Round(value)), Basis: rate.Basis}, nil
}

func costMicros(cost *embeddingeval.CostObservation) int64 {
	if cost == nil {
		return 0
	}
	return cost.Micros
}

type fakeTypeSafeClient struct {
	shape       typesafe.RequestShape
	fingerprint string
	result      typesafe.Result
	requests    []fakeTypeSafeRequest
}

type fakeTypeSafeRequest struct {
	Shape      typesafe.RequestShape
	Query      string
	Candidates []string
}

func (fake *fakeTypeSafeClient) Rerank(_ context.Context, request typesafe.RerankRequest) (typesafe.Result, error) {
	fake.requests = append(fake.requests, fakeTypeSafeRequest{Shape: fake.shape, Query: request.Query, Candidates: slices.Clone(request.Candidates)})
	return fake.result, nil
}

func (fake *fakeTypeSafeClient) RequestShape() typesafe.RequestShape { return fake.shape }

func (fake *fakeTypeSafeClient) PolicyFingerprint() string { return fake.fingerprint }

type fakeCohereClient struct {
	execution cohere.Execution
	requests  []retrieval.RerankingRequest
}

func (fake *fakeCohereClient) RerankWithReceipt(_ context.Context, request retrieval.RerankingRequest) (cohere.Execution, error) {
	fake.requests = append(fake.requests, request)
	return fake.execution, nil
}

func (fake *fakeCohereClient) PolicyFingerprint() string { return "cohere-policy" }

func liveKeysPresent() bool {
	cohereKey, cohereOK := os.LookupEnv("COHERE_API_KEY")
	typesafeKey, typesafeOK := os.LookupEnv("TYPESAFE_API_KEY")
	return cohereOK && strings.TrimSpace(cohereKey) != "" && typesafeOK && strings.TrimSpace(typesafeKey) != ""
}

var _ typeSafeAPI = (*typesafe.Client)(nil)
var _ cohereAPI = (*cohere.Client)(nil)
