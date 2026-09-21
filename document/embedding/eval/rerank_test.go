package eval_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
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
	assert.Nil(t, report.Systems[0].Trials[0].RerankUsage)
}

func TestEvaluateRerankExcerptRollsBackPartialRune(t *testing.T) {
	const asciiBytes = 4095
	candidateText := strings.Repeat("a", asciiBytes) + "界"
	runner := &testRerankingRunner{ranking: []string{"candidate"}, scores: []float64{1}}
	report, err := embeddingeval.Evaluate(context.Background(), embeddingeval.Corpus{
		ID: "excerpt-boundary", Version: "1",
		Documents: []embeddingeval.Document{{ID: "candidate", Text: candidateText}},
		Queries: []embeddingeval.Query{{
			ID: "query", Text: "synthetic query",
			Judgments: []embeddingeval.Judgment{{DocumentID: "candidate", Grade: 1}},
		}},
	}, []embeddingeval.System{{
		ID: "boundary", RecipeFingerprint: "recipe", Reranker: "test",
		RerankerFingerprint: "policy", RerankTopN: 1,
	}}, 1, runner)
	require.NoError(t, err)
	require.Len(t, runner.candidates, 1)
	excerpt := runner.candidates[0].Text
	assert.Len(t, []byte(excerpt), asciiBytes)
	assert.Equal(t, strings.Repeat("a", asciiBytes), excerpt)
	assert.True(t, utf8.ValidString(excerpt))
	assert.Equal(t, []string{"candidate"}, report.Systems[0].Trials[0].Queries[0].Ranking)
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

	for _, emptyQuery := range []string{"q1", "q2"} {
		runner.rankings = map[string][]string{emptyQuery: nil}
		mixed, err := embeddingeval.Evaluate(t.Context(), corpus, []embeddingeval.System{{
			ID: "mixed", RecipeFingerprint: "recipe", Reranker: "test", RerankerFingerprint: "policy", RerankTopN: 2,
		}}, 1, runner)
		require.NoError(t, err)
		observed := mixed.Systems[0]
		assert.Equal(t, &embeddingeval.CostObservation{Micros: 13, Basis: "2026-09-21:base,2026-09-21:rerank"}, observed.Performance.CostPerQuery)
		require.NotNil(t, observed.Trials[0].RerankUsage)
		assert.Equal(t, runner.rerankUsage.Cost, observed.Trials[0].RerankUsage.Cost)
		assert.Equal(t, new(0.75), observed.Performance.RerankTokensPerQuery)
		assert.InDelta(t, 0.5, observed.Performance.RerankRequestsPerQuery, 0.0001)
	}
	runner.rankings = nil

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

func TestEvaluateSkippedRerankPreservesSearchCost(t *testing.T) {
	for _, name := range []string{"search only", "no candidates"} {
		t.Run(name, func(t *testing.T) {
			corpus := rerankCorpus(nil, []string{"hit"})
			system := embeddingeval.System{ID: "search", RecipeFingerprint: "recipe"}
			runner := &testRerankingRunner{ranking: []string{"hit"}, searchUsage: map[string]embeddingeval.Usage{
				"query": {ProviderCalls: 1, Cost: &embeddingeval.CostObservation{Micros: 10, Basis: "2026-09-21:base"}},
			}}
			if name == "no candidates" {
				system.Reranker, system.RerankerFingerprint, system.RerankTopN = "test", "policy", 1
				runner.ranking = nil
			}
			report, err := embeddingeval.Evaluate(t.Context(), corpus, []embeddingeval.System{system}, 1, runner)
			require.NoError(t, err)
			observed := report.Systems[0]
			assert.Equal(t, &embeddingeval.CostObservation{Micros: 10, Basis: "2026-09-21:base"}, observed.Performance.CostPerQuery)
			assert.Nil(t, observed.Trials[0].RerankUsage)
			assert.Nil(t, observed.Trials[0].Queries[0].RerankUsage)
			assert.Equal(t, new(0.0), observed.Performance.RerankTokensPerQuery)
			assert.Zero(t, runner.rerankCalls)
		})
	}
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
			"q1": {ProviderCalls: 1, Cost: &embeddingeval.CostObservation{Micros: costMicros, Basis: "2026-09-21:overflow"}},
			"q2": {ProviderCalls: 1, Cost: &embeddingeval.CostObservation{Micros: costMicros, Basis: "2026-09-21:overflow"}},
		},
	}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "search", RecipeFingerprint: "recipe",
	}}, 1, runner)
	require.NoError(t, err)
	assert.Nil(t, report.Systems[0].Trials[0].Usage.Cost)
	assert.Nil(t, report.Systems[0].Performance.CostPerQuery)
}

func TestEvaluateCostPerQueryMaxInt64(t *testing.T) {
	corpus := embeddingeval.Corpus{
		ID: "cost-boundary", Version: "1",
		Documents: []embeddingeval.Document{{ID: "hit", Text: "synthetic hit"}},
		Queries: []embeddingeval.Query{
			{ID: "q", Text: "query", Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 1}}},
		},
	}
	runner := &testRerankingRunner{
		ranking: []string{"hit"},
		searchUsage: map[string]embeddingeval.Usage{
			"q": {ProviderCalls: 1, Cost: &embeddingeval.CostObservation{Micros: math.MaxInt64, Basis: "2026-09-21:boundary"}},
		},
	}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "search", RecipeFingerprint: "recipe",
	}}, 1, runner)
	require.NoError(t, err)
	require.NotNil(t, report.Systems[0].Performance.CostPerQuery)
	assert.GreaterOrEqual(t, report.Systems[0].Performance.CostPerQuery.Micros, int64(0))
	assert.Equal(t, int64(math.MaxInt64), report.Systems[0].Performance.CostPerQuery.Micros)
}

func TestEvaluateCostPerQueryRoundsHalfUp(t *testing.T) {
	for _, test := range []struct {
		name  string
		costs []int64
		want  int64
	}{
		{name: "3 micros across 2 queries", costs: []int64{1, 2}, want: 2},
		{name: "4 micros across 3 queries", costs: []int64{1, 1, 2}, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, evaluateCostPerQuery(t, test.costs))
		})
	}
}

func evaluateCostPerQuery(t *testing.T, costs []int64) int64 {
	t.Helper()
	corpus := embeddingeval.Corpus{
		ID: "cost-rounding", Version: "1",
		Documents: []embeddingeval.Document{{ID: "hit", Text: "synthetic hit"}},
		Queries:   make([]embeddingeval.Query, len(costs)),
	}
	runner := &testRerankingRunner{
		ranking:     []string{"hit"},
		searchUsage: make(map[string]embeddingeval.Usage, len(costs)),
	}
	for index, cost := range costs {
		queryID := "q" + strconv.Itoa(index+1)
		corpus.Queries[index] = embeddingeval.Query{
			ID: queryID, Text: "query " + queryID,
			Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 1}},
		}
		runner.searchUsage[queryID] = embeddingeval.Usage{
			ProviderCalls: 1,
			Cost:          &embeddingeval.CostObservation{Micros: cost, Basis: "2026-09-21:rounding"},
		}
	}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "search", RecipeFingerprint: "recipe",
	}}, 1, runner)
	require.NoError(t, err)
	require.NotNil(t, report.Systems[0].Performance.CostPerQuery)
	return report.Systems[0].Performance.CostPerQuery.Micros
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

func TestEvaluateCancellation(t *testing.T) {
	for _, test := range []struct {
		name             string
		reranker         string
		cancelBeforeCall bool
		searchCalls      int
	}{
		{name: "rerank before Search", reranker: "test", cancelBeforeCall: true},
		{name: "rerank during successful Search", reranker: "test", searchCalls: 1},
		{name: "search only before Search", cancelBeforeCall: true},
		{name: "search only during successful Search", searchCalls: 1},
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
				ID: "cancel", RecipeFingerprint: "recipe", Reranker: test.reranker, RerankerFingerprint: "policy", RerankTopN: 1,
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
	rankings            map[string][]string
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
	ranking := runner.ranking
	if override, ok := runner.rankings[query.ID]; ok {
		ranking = override
	}
	return embeddingeval.SearchResult{DocumentIDs: slices.Clone(ranking), Usage: usage}, nil
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

func judgmentsFor(ids []string) []embeddingeval.Judgment {
	judgments := make([]embeddingeval.Judgment, len(ids))
	for index, id := range ids {
		judgments[index] = embeddingeval.Judgment{DocumentID: id, Grade: len(ids) - index}
	}
	return judgments
}
