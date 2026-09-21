package eval_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
)

func TestEvaluateReranksBeforeScoring(t *testing.T) {
	corpus := rerankCorpus([]string{"distractor", "relevant"}, []string{"relevant"})
	runner := &testRerankingRunner{ranking: []string{"distractor", "relevant"}, scores: []float64{0.1, 0.9}}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "jev", RecipeFingerprint: "recipe", Reranker: "jev", RerankerFingerprint: "policy", RerankTopN: 2,
	}}, 1, runner)
	require.NoError(t, err)
	query := report.Systems[0].Trials[0].Queries[0]
	assert.Equal(t, []string{"relevant", "distractor"}, query.Ranking)
	assert.InDelta(t, 1.0, query.Metrics.HitAt1, 0.0001)
	assert.InDelta(t, 1.0, report.Systems[0].Aggregate.HitAt1.Mean, 0.0001)
	assert.Greater(t, query.Metrics.NDCGAt10, 0.9)
	assert.Equal(t, 1, runner.rerankCalls)
}

func TestEvaluateRerankPrefix(t *testing.T) {
	ranking := []string{"d0", "d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9", "d10", "d11"}
	corpus := rerankCorpus(ranking, []string{"d0"})
	base := slices.Clone(ranking)
	runner := &testRerankingRunner{
		ranking: ranking,
		scores:  []float64{0.5, 0.5, 0.1, 0.2, 0.3, 0.4, 0.6, 0.7, 0.8, 0.9},
	}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "prefix", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 10,
	}}, 1, runner)
	require.NoError(t, err)
	assert.Equal(t, base, runner.ranking)
	assert.Equal(t, []string{"d9", "d8", "d7", "d6", "d0", "d1", "d5", "d4", "d3", "d2", "d10", "d11"}, report.Systems[0].Trials[0].Queries[0].Ranking)
	require.Len(t, runner.candidates, 10)
	assert.LessOrEqual(t, len([]byte(runner.candidates[0].Text)), 4096)
	assert.True(t, utf8.ValidString(runner.candidates[0].Text))
	assert.Equal(t, "d0", runner.candidates[0].ID)

	runner = &testRerankingRunner{ranking: []string{"d0", "d1"}, scores: []float64{0.1, 0.9}}
	report, err = embeddingeval.Evaluate(context.Background(), rerankCorpus([]string{"d0", "d1"}, []string{"d1"}), []embeddingeval.System{{
		ID: "above-count", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 10,
	}}, 1, runner)
	require.NoError(t, err)
	assert.Equal(t, []string{"d1", "d0"}, report.Systems[0].Trials[0].Queries[0].Ranking)

	runner = &testRerankingRunner{ranking: nil}
	report, err = embeddingeval.Evaluate(context.Background(), rerankCorpus(nil, []string{"d0"}), []embeddingeval.System{{
		ID: "empty", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 10,
	}}, 1, runner)
	require.NoError(t, err)
	assert.Zero(t, runner.rerankCalls)
	assert.Equal(t, 0, report.Systems[0].Trials[0].RerankUsage.ProviderCalls)
}

func TestEvaluateQueryObservations(t *testing.T) {
	corpus := embeddingeval.Corpus{
		ID: "metrics", Version: "1",
		Documents: []embeddingeval.Document{{ID: "hit", Text: "hit text"}, {ID: "miss", Text: "miss text"}},
		Queries: []embeddingeval.Query{
			{ID: "q1", Text: "one", Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 2}}},
			{ID: "q2", Text: "two", Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 2}}},
		},
	}
	tokenUsage := func(value float64) *float64 { return &value }
	runner := &testRerankingRunner{
		ranking: []string{"hit", "miss"}, scores: []float64{0.9, 0.1},
		searchUsage: map[string]embeddingeval.Usage{
			"q1": {ProviderCalls: 1, Latency: 10 * time.Millisecond, TokenUsage: tokenUsage(4.5), Cost: &embeddingeval.CostObservation{Micros: 10, Basis: "2026-09-21:base"}},
			"q2": {ProviderCalls: 1, Latency: 30 * time.Millisecond, TokenUsage: tokenUsage(4.5), Cost: &embeddingeval.CostObservation{Micros: 10, Basis: "2026-09-21:base"}},
		},
		rerankUsage: embeddingeval.Usage{ProviderCalls: 1, Latency: 5 * time.Millisecond, TokenUsage: tokenUsage(1.5), Cost: &embeddingeval.CostObservation{Micros: 5, Basis: "2026-09-21:rerank"}},
	}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "metrics", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 2,
	}}, 2, runner)
	require.NoError(t, err)
	performance := report.Systems[0].Performance
	assert.Equal(t, 4, performance.QueryCount)
	assert.Equal(t, 35*time.Millisecond, performance.P95Latency)
	assert.InDelta(t, 2.0, performance.RequestsPerQuery, 0.0001)
	assert.InDelta(t, 1.0, performance.RerankRequestsPerQuery, 0.0001)
	require.NotNil(t, performance.TokensPerQuery)
	assert.InDelta(t, 6.0, *performance.TokensPerQuery, 0.0001)
	require.NotNil(t, performance.RerankTokensPerQuery)
	assert.InDelta(t, 1.5, *performance.RerankTokensPerQuery, 0.0001)
	require.NotNil(t, performance.CostPerQuery)
	assert.Equal(t, int64(15), performance.CostPerQuery.Micros)
	assert.Equal(t, "2026-09-21:base,2026-09-21:rerank", performance.CostPerQuery.Basis)

	runner.searchUsage["q2"] = embeddingeval.Usage{Latency: 30 * time.Millisecond}
	report, err = embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "missing", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 2,
	}}, 1, runner)
	require.NoError(t, err)
	assert.Nil(t, report.Systems[0].Performance.TokensPerQuery)
	assert.Nil(t, report.Systems[0].Performance.CostPerQuery)
	assert.Nil(t, report.Systems[0].Trials[0].Usage.TokenUsage)
	assert.Nil(t, report.Systems[0].Trials[0].Usage.Cost)
}

func TestEvaluateCostAggregationOverflow(t *testing.T) {
	const costMicros = int64(1 << 62)
	corpus := embeddingeval.Corpus{
		ID: "cost-overflow", Version: "1",
		Documents: []embeddingeval.Document{{ID: "hit", Text: "synthetic hit"}},
		Queries: []embeddingeval.Query{
			{ID: "q1", Text: "first", Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 1}}},
			{ID: "q2", Text: "second", Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 1}}},
		},
	}
	runner := &testRerankingRunner{
		ranking: []string{"hit"},
		searchUsage: map[string]embeddingeval.Usage{
			"q1": {ProviderCalls: 1, EstimatedCostMicros: costMicros, Cost: &embeddingeval.CostObservation{Micros: costMicros, Basis: "2026-09-21:overflow"}},
			"q2": {ProviderCalls: 1, EstimatedCostMicros: costMicros, Cost: &embeddingeval.CostObservation{Micros: costMicros, Basis: "2026-09-21:overflow"}},
		},
	}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "search", RecipeFingerprint: "recipe",
	}}, 1, runner)
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), report.Systems[0].Trials[0].Usage.EstimatedCostMicros)
	assert.Nil(t, report.Systems[0].Trials[0].Usage.Cost)
	assert.Nil(t, report.Systems[0].Performance.CostPerQuery)
}

func TestEvaluateRerankModesAndFailures(t *testing.T) {
	corpus := rerankCorpus([]string{"d0", "d1"}, []string{"d1"})
	legacy := &testRerankingRunner{ranking: []string{"d0", "d1"}, scores: []float64{0.1, 0.9}}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "empty", RecipeFingerprint: "recipe",
	}}, 1, legacy)
	require.NoError(t, err)
	assert.Zero(t, legacy.rerankCalls)
	assert.Equal(t, 1, report.Systems[0].Trials[0].Usage.ProviderCalls)

	for _, test := range []struct {
		name   string
		system embeddingeval.System
		runner embeddingeval.Runner
	}{
		{name: "missing fingerprint", system: embeddingeval.System{ID: "bad", RecipeFingerprint: "recipe", Reranker: "test", RerankTopN: 1}, runner: legacy},
		{name: "missing capability", system: embeddingeval.System{ID: "bad", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 1}, runner: staticRunnerForRerank{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{test.system}, 1, test.runner)
			require.Error(t, err)
		})
	}

	for _, topN := range []int{0, -1} {
		runner := &testRerankingRunner{ranking: []string{"d0", "d1"}, scores: []float64{0.1}}
		_, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
			ID: "invalid-top-n", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: topN,
		}}, 1, runner)
		require.Error(t, err)
		assert.Zero(t, runner.searchCalls)
	}

	providerErr := errors.New("synthetic provider failure")
	runner := &testRerankingRunner{ranking: []string{"d0", "d1"}, rerankErr: providerErr}
	_, err = embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "failure", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 1,
	}}, 1, runner)
	require.ErrorIs(t, err, providerErr)

	for _, scores := range [][]float64{{}, {math.NaN()}} {
		runner = &testRerankingRunner{ranking: []string{"d0", "d1"}, scores: scores}
		_, err = embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
			ID: "malformed", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 1,
		}}, 1, runner)
		require.Error(t, err)
	}

	for _, usage := range []embeddingeval.Usage{
		{TokenUsage: new(-1.0)},
		{TokenUsage: new(math.NaN())},
		{TokenUsage: new(math.Inf(1))},
		{TokenUsage: new(math.Inf(-1))},
		{Cost: &embeddingeval.CostObservation{Micros: -1, Basis: "2026-09-21:rate"}},
		{Cost: &embeddingeval.CostObservation{Micros: 1}},
	} {
		runner = &testRerankingRunner{ranking: []string{"d0", "d1"}, scores: []float64{0.1}, rerankUsage: usage}
		_, err = embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
			ID: "invalid-usage", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 1,
		}}, 1, runner)
		require.Error(t, err)
	}

	cancelContext, cancel := context.WithCancel(context.Background())
	canceled := &testRerankingRunner{ranking: []string{"d0", "d1"}, scores: []float64{0.1}, cancelContext: cancel}
	_, err = embeddingeval.Evaluate(cancelContext, corpus, []embeddingeval.System{{
		ID: "cancel", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 1,
	}}, 1, canceled)
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, canceled.rerankContextErr, context.Canceled)
}

func TestEvaluateSearchOnlyPreservesContextBehavior(t *testing.T) {
	corpus := rerankCorpus([]string{"d0"}, []string{"d0"})
	runner := &contextBehaviorRunner{}
	var nilContext context.Context
	_, err := embeddingeval.Evaluate(nilContext, corpus, []embeddingeval.System{{ID: "legacy", RecipeFingerprint: "recipe"}}, 1, runner)
	require.NoError(t, err)
	assert.True(t, runner.sawNilContext)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = embeddingeval.Evaluate(ctx, corpus, []embeddingeval.System{{ID: "legacy", RecipeFingerprint: "recipe"}}, 1, runner)
	require.ErrorIs(t, err, runner.canceledError)
	assert.True(t, runner.sawCanceledContext)
}

func TestEvaluateNamedRerankerCancellation(t *testing.T) {
	for _, test := range []struct {
		name             string
		cancelBeforeCall bool
		searchCalls      int
	}{
		{name: "before Search", cancelBeforeCall: true},
		{name: "during successful Search", searchCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			runner := &testRerankingRunner{
				ranking:             []string{"d0", "d1"},
				scores:              []float64{0.9},
				cancelSearchContext: cancel,
			}
			if test.cancelBeforeCall {
				cancel()
			}
			report, err := embeddingeval.Evaluate(ctx, rerankCorpus(runner.ranking, []string{"d0"}), []embeddingeval.System{{
				ID: "cancel", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 1,
			}}, 1, runner)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, embeddingeval.Report{}, report)
			assert.Equal(t, test.searchCalls, runner.searchCalls)
			assert.Zero(t, runner.rerankCalls)
		})
	}
}

type testRerankingRunner struct {
	ranking             []string
	scores              []float64
	searchUsage         map[string]embeddingeval.Usage
	rerankUsage         embeddingeval.Usage
	rerankErr           error
	cancelContext       context.CancelFunc
	cancelSearchContext context.CancelFunc
	rerankContextErr    error
	searchCalls         int
	rerankCalls         int
	candidates          []embeddingeval.Document
}

func (runner *testRerankingRunner) Search(_ context.Context, _ embeddingeval.System, _ embeddingeval.Corpus, query embeddingeval.Query) (embeddingeval.SearchResult, error) {
	runner.searchCalls++
	usage := runner.searchUsage[query.ID]
	if usage.ProviderCalls == 0 && usage.Latency == 0 && usage.TokenUsage == nil && usage.Cost == nil {
		usage.ProviderCalls = 1
	}
	if runner.cancelSearchContext != nil {
		runner.cancelSearchContext()
	}
	return embeddingeval.SearchResult{DocumentIDs: slices.Clone(runner.ranking), Usage: usage}, nil
}

func (runner *testRerankingRunner) Rerank(ctx context.Context, _ embeddingeval.System, _ string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error) {
	runner.rerankCalls++
	runner.candidates = slices.Clone(candidates)
	if runner.cancelContext != nil {
		runner.cancelContext()
		runner.rerankContextErr = ctx.Err()
		return embeddingeval.RerankResult{Scores: slices.Clone(runner.scores), Usage: runner.rerankUsage}, nil
	}
	if runner.rerankErr != nil {
		return embeddingeval.RerankResult{}, runner.rerankErr
	}
	return embeddingeval.RerankResult{Scores: slices.Clone(runner.scores), Usage: runner.rerankUsage}, nil
}

type staticRunnerForRerank struct{}

func (staticRunnerForRerank) Search(context.Context, embeddingeval.System, embeddingeval.Corpus, embeddingeval.Query) (embeddingeval.SearchResult, error) {
	return embeddingeval.SearchResult{}, nil
}

func rerankCorpus(ranking, relevant []string) embeddingeval.Corpus {
	documents := make([]embeddingeval.Document, 0, max(len(ranking), len(relevant)))
	seen := make(map[string]bool)
	for _, id := range append(slices.Clone(ranking), relevant...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		text := "synthetic text for " + id
		if id == "d0" {
			text += " " + strings.Repeat("界", 2000)
		}
		documents = append(documents, embeddingeval.Document{ID: id, Text: text})
	}
	return embeddingeval.Corpus{ID: "synthetic", Version: "1", Documents: documents, Queries: []embeddingeval.Query{{
		ID: "query", Text: "synthetic query", Judgments: judgmentsFor(relevant),
	}}}
}

type contextBehaviorRunner struct {
	sawNilContext      bool
	sawCanceledContext bool
	canceledError      error
}

func (runner *contextBehaviorRunner) Search(ctx context.Context, _ embeddingeval.System, _ embeddingeval.Corpus, query embeddingeval.Query) (embeddingeval.SearchResult, error) {
	if ctx == nil {
		runner.sawNilContext = true
	} else if err := ctx.Err(); err != nil {
		runner.sawCanceledContext = true
		if runner.canceledError == nil {
			runner.canceledError = errors.New("legacy runner observed cancellation")
		}
		return embeddingeval.SearchResult{}, runner.canceledError
	}
	return embeddingeval.SearchResult{DocumentIDs: []string{query.Judgments[0].DocumentID}}, nil
}

func judgmentsFor(ids []string) []embeddingeval.Judgment {
	judgments := make([]embeddingeval.Judgment, len(ids))
	for index, id := range ids {
		judgments[index] = embeddingeval.Judgment{DocumentID: id, Grade: len(ids) - index}
	}
	return judgments
}
