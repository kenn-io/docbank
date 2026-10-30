package rerankeval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tiktoken-go/tokenizer"
	"go.kenn.io/docbank/document"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
)

func TestPassageVoyageAccountsAttemptAndRejectsMalformedScores(t *testing.T) {
	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	require.NoError(t, err)
	body := `{"data":[{"index":1,"relevance_score":0.8},{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":7}}`
	var received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Documents  []string `json:"documents"`
			Truncation *bool    `json:"truncation"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		assert.Equal(t, []string{"first", "second"}, request.Documents)
		if assert.NotNil(t, request.Truncation) {
			assert.False(t, *request.Truncation)
		}
		received++
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	runner := &passageVoyageRunner{endpoint: server.URL, client: server.Client(), secret: func() string { return "synthetic" }, codec: codec, budget: &passageBudget{started: time.Now()}}
	inputs := []embeddingeval.PassageInput{{DocumentID: "a", Text: "first"}, {DocumentID: "b", Passage: 1, Text: "second"}}
	got, err := runner.RerankPassages(t.Context(), "query", inputs)
	require.NoError(t, err)
	require.Len(t, got.Scores, 2)
	require.Equal(t, []string{"b", "a"}, []string{got.Scores[0].DocumentID, got.Scores[1].DocumentID})
	require.Equal(t, []int{1, 0}, []int{got.Scores[0].Passage, got.Scores[1].Passage})
	require.InDeltaSlice(t, []float64{.8, .5}, []float64{got.Scores[0].Score, got.Scores[1].Score}, 1e-12)
	require.InDelta(t, 7.0, *got.Usage.TokenUsage, 1e-12)
	require.Equal(t, 4, got.LocalTokens)
	require.Greater(t, got.RequestBytes, len("firstsecond"))
	body = `{"data":[{"index":0},{"index":1,"relevance_score":2}]}`
	got, err = runner.RerankPassages(t.Context(), "query", inputs)
	require.Error(t, err, "missing score cannot silently become zero")
	require.Positive(t, got.RequestBytes)
	require.Equal(t, 2, received)
	before := received
	runner.budget.EstimatedMicros = 25_000_000
	_, err = runner.RerankPassages(t.Context(), "query", inputs)
	require.Error(t, err)
	require.Equal(t, before, received, "budget rejects before sending")
}

func TestPassagePreparationUsesOwningServiceSearch(t *testing.T) {
	profile, err := passageEmbeddingProfile()
	require.NoError(t, err)
	corpus := embeddingeval.Corpus{ID: "synthetic", Version: "1", Documents: []embeddingeval.Document{{ID: "a", Text: "clantern " + strings.Repeat("界", 600) + " lantern archive answer. " + strings.Repeat("界", 100)}}, Queries: []embeddingeval.Query{{ID: "q", Text: "lantern archive", Judgments: []embeddingeval.Judgment{{DocumentID: "a", Grade: 1}}}}}
	fixture, err := preparePassageCorpus(t.Context(), t.TempDir(), corpus, &passageFakeEmbeddings{descriptor: profile.Descriptor})
	require.NoError(t, err)
	defer func() { require.NoError(t, fixture.close()) }()
	prepared, err := fixture.prepare(t.Context(), corpus.Queries[0])
	require.NoError(t, err)
	report, err := fixture.service.Search(t.Context(), fixture.request("lantern archive"))
	require.NoError(t, err)
	require.Len(t, prepared.Candidates, 1)
	require.Equal(t, report.Results[0].Excerpt, prepared.Candidates[0].Excerpt)
	selected, diagnostics, err := prepared.Select(t.Context(), 3)
	require.NoError(t, err)
	require.NotEmpty(t, selected[0].Passages)
	require.Positive(t, diagnostics.Chunks+diagnostics.Lexical)
	require.NotEqual(t, corpus.Documents[0].Text, prepared.Candidates[0].Excerpt)
	for _, query := range []string{"lantern", "lantern?", "lant?", "lantern? archive?", "lantern? missing?"} {
		t.Run(query, func(t *testing.T) {
			prepared, err := fixture.prepare(t.Context(), embeddingeval.Query{Text: query})
			require.NoError(t, err)
			selected, diagnostics, err := prepared.Select(t.Context(), 3)
			require.NoError(t, err)
			wantLexical := 1
			if query == "lantern? missing?" {
				wantLexical = 0
			}
			require.Equal(t, wantLexical, diagnostics.Lexical)
			require.Equal(t, 1, diagnostics.Chunks)
			require.Len(t, selected[0].Passages, wantLexical+1)
			for _, passage := range selected[0].Passages {
				require.Contains(t, passage, " lantern archive answer")
				require.LessOrEqual(t, len(passage), 1536)
			}
		})
	}
}

type passageFakeEmbeddings struct {
	descriptor document.EmbeddingDescriptor
	fail       bool
	calls      int
	queryCalls int
}

func (p *passageFakeEmbeddings) Descriptor() document.EmbeddingDescriptor { return p.descriptor }
func (p *passageFakeEmbeddings) Embed(_ context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	p.calls++
	if p.fail {
		return document.EmbeddingResult{}, errors.New("synthetic embedding failure")
	}
	var result document.EmbeddingResult
	for _, input := range inputs {
		if input.Role == document.EmbeddingRoleQuery {
			p.queryCalls++
		}
		values := make([]float32, p.descriptor.Dimension)
		values[0] = 1
		result.Vectors = append(result.Vectors, document.EmbeddingVector{Key: input.Key, Values: values})
	}
	return result, nil
}

func TestPassageBootstrapAndIncompleteGate(t *testing.T) {
	require.Equal(t, [2]float64{0.25, 0.25}, passageInterval([]float64{0.25, 0.25, 0.25}))
	// Binomial(10, 0.5) bootstrap means have exact 95% bounds of 0.2 and 0.8.
	interval := passageInterval([]float64{0, 0, 0, 0, 0, 1, 1, 1, 1, 1})
	require.InDeltaSlice(t, []float64{0.2, 0.8}, interval[:], 1e-12)
}

func passageLongCorpus() embeddingeval.Corpus {
	corpus := embeddingeval.Corpus{ID: "synthetic-long", Version: "20261030v1"}
	for i := range 24 {
		text := strings.Repeat("Ordinary synthetic filing discusses routine maintenance. ", 200) + fmt.Sprintf("Lantern archive record %d states that its access key is cerulean.", i)
		corpus.Documents = append(corpus.Documents, embeddingeval.Document{ID: strconv.Itoa(i), Text: text})
	}
	for i := range 4 {
		corpus.Queries = append(corpus.Queries, embeddingeval.Query{ID: strconv.Itoa(i), Text: fmt.Sprintf("lantern archive record %d access key", i), Judgments: []embeddingeval.Judgment{{DocumentID: strconv.Itoa(i), Grade: 1}}})
	}
	return corpus
}

func TestPassageGateRequiresEveryQueryAndControl(t *testing.T) {
	report := passageGateReport{}
	report.finish()
	require.Equal(t, "inconclusive", report.Summary.Decision)
	arm := embeddingeval.PassageArm{InputBytes: 200, LogicalBytes: 220, LocalTokens: 60, RequestBytes: 1024, Usage: embeddingeval.Usage{Latency: time.Millisecond}}
	// All arms permute the same candidates; document 0 is the sole positive.
	rankedArm := func(rank int) embeddingeval.PassageArm {
		value := arm
		for i := range 20 {
			value.Ranking = append(value.Ranking, strconv.Itoa(i))
		}
		value.Ranking[0], value.Ranking[rank-1] = value.Ranking[rank-1], value.Ranking[0]
		return value
	}
	score := func(observation *passageObservation) {
		metrics, err := embeddingeval.ScorePassageComparison(observation.Comparison, []embeddingeval.Judgment{{DocumentID: "0", Grade: 1}}, 5)
		require.NoError(t, err)
		observation.Metrics = metrics
		observation.NDCGAt10 = [4]float64{metrics.Baseline.NDCGAt10, metrics.Current.NDCGAt10, metrics.P1.NDCGAt10, metrics.P3.NDCGAt10}
		observation.HitAt1 = [4]float64{metrics.Baseline.HitAt1, metrics.Current.HitAt1, metrics.P1.HitAt1, metrics.P3.HitAt1}
	}
	observations := []passageObservation{}
	for _, sample := range []struct {
		name  string
		count int
	}{{"public", 100}, {"short", 2}, {"synthetic", 4}} {
		for query := range sample.count {
			for repeat := range 3 {
				current, p1 := 2+query%2, 2+query%2
				if repeat == 0 {
					current = 1
				}
				if repeat < 2 {
					p1 = 1
				}
				observation := passageObservation{
					Corpus: sample.name, Query: strconv.Itoa(query), Repeat: repeat, Proxy: query%2 == 0,
					Comparison: embeddingeval.PassageComparison{Baseline: rankedArm(3 - query%2).Ranking, Current: rankedArm(current), P1: rankedArm(p1), P3: rankedArm(1)},
				}
				score(&observation)
				observations = append(observations, observation)
			}
		}
	}
	report = passageGateReport{Observations: observations}
	report.finish()
	require.Equal(t, "pass", report.Summary.Decision)
	encoded, err := json.MarshalIndent(report.Summary, "", "  ")
	require.NoError(t, err)
	var published passageGateSummary
	require.NoError(t, json.Unmarshal(encoded, &published))
	for _, corpus := range []string{"public", "proxy", "short", "synthetic"} {
		wantNDCG := []float64{.5654648767857288, .7103099178571525, .8551549589285762, 1}
		if corpus == "proxy" {
			wantNDCG = []float64{.5, .7539531690476383, .8769765845238192, 1}
		}
		for arm, name := range []string{"no-rerank", "current", "p1", "p3"} {
			quality := published.Quality[corpus][arm]
			require.Equal(t, name, quality.Name)
			require.InDelta(t, wantNDCG[arm], quality.NDCGAt10, 1e-12)
			require.InDelta(t, []float64{0, 1.0 / 3, 2.0 / 3, 1}[arm], quality.HitAt1, 1e-12)
		}
	}
	markdown, err := report.markdown()
	require.NoError(t, err)
	require.Contains(t, markdown, string(encoded), "Markdown must publish the same four-arm values")
	require.Contains(t, markdown, "critical-span evidence: unavailable")
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CriticalMisses", "unknown evidence must not serialize as zero")
	report.Summary.Arms[0].MeanJSONBytes = math.NaN()
	_, err = report.markdown()
	require.Error(t, err, "nonfinite evidence must not silently publish empty JSON")
	for _, missing := range []string{"public", "short", "proxy"} {
		incomplete := make([]passageObservation, 0, len(observations))
		for _, observation := range observations {
			if observation.Corpus == missing && observation.Query == "0" {
				continue
			}
			if missing == "proxy" {
				query, err := strconv.Atoi(observation.Query)
				require.NoError(t, err)
				observation.Proxy = query < 29
			}
			incomplete = append(incomplete, observation)
		}
		report = passageGateReport{Observations: incomplete}
		report.finish()
		require.Equal(t, "inconclusive", report.Summary.Decision, missing)
	}
	for arm, selected := range []*embeddingeval.PassageArm{&observations[0].Comparison.P1, &observations[0].Comparison.P3} {
		selected.Selection.Capped = true
		report = passageGateReport{Observations: observations}
		report.finish()
		require.Equal(t, "inconclusive", report.Summary.Decision)
		require.True(t, report.Summary.Arms[arm].Selection.Capped, "aggregate must disclose the cap that makes the gate inconclusive")
		selected.Selection.Capped = false
	}
	report = passageGateReport{Observations: observations[:len(observations)-3]}
	report.finish()
	require.Equal(t, "inconclusive", report.Summary.Decision, "all four synthetic control queries are required")
	for i := range observations {
		if observations[i].Corpus == "synthetic" {
			observations[i].Comparison.P3 = rankedArm(20)
			score(&observations[i])
		}
	}
	report = passageGateReport{Observations: observations}
	report.finish()
	require.Equal(t, "fail", report.Summary.Decision, "live control loss must gate the result")
	report = passageGateReport{Observations: observations[:len(observations)-1]}
	report.finish()
	require.Equal(t, "inconclusive", report.Summary.Decision)
}

func TestPassageSyntheticDriverKeepsFrozenBaselineAcrossRepeats(t *testing.T) {
	profile, err := passageEmbeddingProfile()
	require.NoError(t, err)
	report := passageGateReport{}
	provider := &passageFakeEmbeddings{descriptor: profile.Descriptor}
	corpus := passageLongCorpus()
	err = runPassageCorpus(t.Context(), t.TempDir(), "synthetic", corpus, provider, passageConstantRunner{}, &report)
	require.NoError(t, err)
	require.Equal(t, 4, provider.queryCalls, "each query is prepared once across three repeats")
	require.Len(t, report.Observations, 12)
	for i, o := range report.Observations {
		require.Empty(t, o.Failure)
		require.Len(t, o.Comparison.Baseline, 20)
		require.Equal(t, report.Observations[i-i%3].Comparison.Baseline, o.Comparison.Baseline)
	}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	var saved struct {
		Versions map[string]map[string]string `json:"Versions"`
	}
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.Len(t, saved.Versions["synthetic"], 24, "external observations retain the exact corpus-to-vault mapping")
	for _, doc := range corpus.Documents {
		require.NotEmpty(t, saved.Versions["synthetic"][doc.ID])
	}
	for _, version := range report.Observations[0].Comparison.Baseline {
		require.Contains(t, slices.Collect(maps.Values(saved.Versions["synthetic"])), version)
	}
}

type passageConstantRunner struct{ after func() }

func (r passageConstantRunner) RerankPassages(_ context.Context, _ string, inputs []embeddingeval.PassageInput) (embeddingeval.PassageScores, error) {
	scores := make([]embeddingeval.PassageScore, len(inputs))
	for i, input := range inputs {
		scores[i] = embeddingeval.PassageScore{DocumentID: input.DocumentID, Passage: input.Passage, Score: 1}
	}
	if r.after != nil {
		r.after()
	}
	return embeddingeval.PassageScores{Scores: scores}, nil
}

func TestPassageCancellationPreservesPartialReportAndClosesFixture(t *testing.T) {
	profile, err := passageEmbeddingProfile()
	require.NoError(t, err)
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
	defer stop()
	corpus := embeddingeval.Corpus{Documents: []embeddingeval.Document{{ID: "a", Text: "lantern archive answer"}}, Queries: []embeddingeval.Query{{ID: "first", Text: "lantern", Judgments: []embeddingeval.Judgment{{DocumentID: "a", Grade: 1}}}, {ID: "interrupted", Text: "archive", Judgments: []embeddingeval.Judgment{{DocumentID: "a", Grade: 1}}}}}
	root, output := t.TempDir(), t.TempDir()
	report := passageGateReport{}
	calls := 0
	err = runPassageCorpus(ctx, root, "short", corpus, &passageFakeEmbeddings{descriptor: profile.Descriptor}, passageConstantRunner{after: func() {
		calls++
		if calls == 10 {
			stop() // NotifyContext's stop cancels portably, including on Windows.
		}
	}}, &report)
	require.NoError(t, err)
	writePassageGateReport(t, output, &passageBudget{started: time.Now()}, &report)
	data, err := os.ReadFile(filepath.Join(output, "observations.json"))
	require.NoError(t, err)
	var saved passageGateReport
	require.NoError(t, json.Unmarshal(data, &saved))
	require.Len(t, saved.Observations, 6)
	require.Empty(t, saved.Observations[0].Failure)
	require.Len(t, saved.Observations[0].Comparison.Current.Ranking, 1)
	require.Contains(t, saved.Observations[3].Failure, "context canceled")
	require.Equal(t, "inconclusive", saved.Summary.Decision)
	require.NoError(t, os.RemoveAll(root), "fixture shutdown must release its files on Windows")
}

func TestPassagePreparationDoesNotRetryFailure(t *testing.T) {
	profile, err := passageEmbeddingProfile()
	require.NoError(t, err)
	provider := &passageFakeEmbeddings{descriptor: profile.Descriptor, fail: true}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = preparePassageCorpus(ctx, t.TempDir(), embeddingeval.Corpus{Documents: []embeddingeval.Document{{ID: "a", Text: "synthetic failure input"}}}, provider)
	require.EqualError(t, err, "embedding preparation incomplete: state=failed phase=embedding code=input_rejected")
	require.Equal(t, 1, provider.calls, "a failed preparation call must be visible without retries")
}

func TestPassageLiveOutputCollisionPreservesEvidence(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"observations.json", "aggregate.json", "report.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("retained complete evidence"), 0600))
	}
	binary, err := os.Executable()
	require.NoError(t, err)
	repo, err := filepath.Abs("../..")
	require.NoError(t, err)
	for _, test := range []struct{ output, failure string }{
		{dir, "output directory must not exist"},
		{repo, "output must be outside the repository"},
		{filepath.Join(repo, ".superpowers", "rejected-output"), "output must be outside the repository"},
		{"relative-output", "output must be absolute"},
	} {
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestLivePassageGate$", "-test.count=1")
		cmd.Env = []string{"RERANK_EVAL_LIVE=1", "RERANK_PASSAGE_OUTPUT=" + test.output, "RERANK_PASSAGE_CORPUS=" + filepath.Join(dir, "missing-corpus.json")}
		output, err := cmd.CombinedOutput()
		require.Error(t, err)
		require.Contains(t, string(output), test.failure)
		require.NotContains(t, string(output), "missing-corpus.json")
	}
	for _, name := range []string{"observations.json", "aggregate.json", "report.md"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, "retained complete evidence", string(got))
	}
}
