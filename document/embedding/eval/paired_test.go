package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPairedRerankUsesFrozenCandidatesAndActualExcerpts(t *testing.T) {
	candidates := []PassageCandidate{
		{DocumentID: "a", Excerpt: "actual snippet a", Passages: []string{"segment a1", "segment a2"}},
		{DocumentID: "b", Excerpt: "actual snippet b", Passages: []string{"segment b1"}},
	}
	runner := &pairedTestRunner{}
	result, err := EvaluatePassages(context.Background(), "query", candidates, runner)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, result.Baseline)
	require.Equal(t, []string{"b", "a"}, result.Current.Ranking)
	require.Equal(t, []string{"b", "a"}, result.P1.Ranking)
	require.Equal(t, []string{"a", "b"}, result.P3.Ranking)
	require.Equal(t, [][]string{{"actual snippet a", "actual snippet b"}, {"segment a1", "segment b1"}, {"segment a1", "segment a2", "segment b1"}}, runner.inputs)
}

type pairedTestRunner struct{ inputs [][]string }

func (r *pairedTestRunner) RerankPassages(_ context.Context, _ string, candidates []PassageInput) (PassageScores, error) {
	texts := make([]string, len(candidates))
	scores := make([]PassageScore, len(candidates))
	for i, c := range candidates {
		texts[i] = c.Text
		value := 0.1
		switch c.Text {
		case "actual snippet b", "segment a2":
			value = 0.9
		case "segment b1":
			value = 0.5
		}
		scores[i] = PassageScore{DocumentID: c.DocumentID, Passage: c.Passage, Score: value}
	}
	r.inputs = append(r.inputs, texts)
	return PassageScores{Scores: scores}, nil
}

func TestPairedRerankRejectsInvalidScores(t *testing.T) {
	for _, scores := range [][]PassageScore{
		nil,
		{{DocumentID: "a", Passage: 0, Score: math.NaN()}, {DocumentID: "b", Passage: 0, Score: 1}},
		{{DocumentID: "other", Passage: 0, Score: 1}, {DocumentID: "b", Passage: 0, Score: 1}},
		{{DocumentID: "a", Passage: 0, Score: 1}, {DocumentID: "a", Passage: 0, Score: 2}},
	} {
		_, err := EvaluatePassages(context.Background(), "q", []PassageCandidate{{DocumentID: "a", Excerpt: "first"}, {DocumentID: "b", Excerpt: "second"}}, fixedPassageRunner{scores})
		require.Error(t, err)
	}
}

type fixedPassageRunner struct{ scores []PassageScore }

func (r fixedPassageRunner) RerankPassages(context.Context, string, []PassageInput) (PassageScores, error) {
	return PassageScores{Scores: r.scores}, nil
}

func FuzzPairedRejectsNonfiniteAndKeepsMax(f *testing.F) {
	f.Add(0.2, 0.9, 0.5)
	f.Add(-1.0, -2.0, -1.5)
	f.Fuzz(func(t *testing.T, a, b, c float64) {
		candidates := []PassageCandidate{{DocumentID: "a", Excerpt: "x", Passages: []string{"x", "y"}}, {DocumentID: "b", Excerpt: "z", Passages: []string{"z"}}}
		runner := valuePassageRunner{a, b, c}
		comparison, err := EvaluatePassages(t.Context(), "q", candidates, runner)
		if math.IsNaN(a) || math.IsInf(a, 0) || math.IsNaN(b) || math.IsInf(b, 0) || math.IsNaN(c) || math.IsInf(c, 0) {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		want := []string{"a", "b"}
		if c > max(a, b) {
			want = []string{"b", "a"}
		}
		require.Equal(t, want, comparison.P3.Ranking)
	})
}

type valuePassageRunner struct{ a, b, c float64 }

func (r valuePassageRunner) RerankPassages(_ context.Context, _ string, inputs []PassageInput) (PassageScores, error) {
	var scores []PassageScore
	for _, input := range inputs {
		value := r.a
		if input.DocumentID == "b" {
			value = r.c
		} else if input.Passage == 1 {
			value = r.b
		}
		scores = append(scores, PassageScore{DocumentID: input.DocumentID, Passage: input.Passage, Score: value})
	}
	return PassageScores{Scores: scores}, nil
}

func TestPairedRejectsInvalidCandidateAndPreservesFallback(t *testing.T) {
	for _, candidates := range [][]PassageCandidate{
		{{DocumentID: "a", Excerpt: "x"}, {DocumentID: "a", Excerpt: "y"}},
		{{DocumentID: "a", Excerpt: string([]byte{0xff})}},
		{{DocumentID: "a", Excerpt: strings.Repeat("x", 4097)}},
		{{DocumentID: "a", Excerpt: "x", Passages: []string{strings.Repeat("界", 1366)}}},
	} {
		runner := &pairedTestRunner{}
		_, err := EvaluatePassages(t.Context(), "q", candidates, runner)
		require.Error(t, err)
		require.Empty(t, runner.inputs, "invalid candidates must be rejected before a provider call")
	}
	runner := &pairedTestRunner{}
	_, err := EvaluatePassages(t.Context(), "q", []PassageCandidate{{DocumentID: "a", Excerpt: "fallback"}}, runner)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"fallback"}, {"fallback"}, {"fallback"}}, runner.inputs)
}

func TestPassageComparisonMetricsUseCriticalCutoffInsideCandidates(t *testing.T) {
	comparison := PassageComparison{Baseline: []string{"a", "b", "c"}, Current: PassageArm{Ranking: []string{"c", "a", "b"}}, P1: PassageArm{Ranking: []string{"b", "c", "a"}}, P3: PassageArm{Ranking: []string{"c", "b", "a"}}}
	metrics, err := ScorePassageComparison(comparison, []Judgment{{DocumentID: "c", Grade: 1, Critical: true}}, 2)
	require.NoError(t, err)
	require.Equal(t, 1, metrics.Baseline.CriticalMisses)
	require.Equal(t, 0, metrics.Current.CriticalMisses)
	require.Equal(t, 0, metrics.P1.CriticalMisses)
	require.Equal(t, 0, metrics.P3.CriticalMisses)
	require.InDelta(t, 1.0, metrics.Current.HitAt1, 0.0001)
	_, err = ScorePassageComparison(comparison, nil, 20)
	require.Error(t, err)
}

func TestPassageComparisonMetricsRejectsChangedCandidateSet(t *testing.T) {
	good := PassageArm{Ranking: []string{"a", "b"}}
	for _, bad := range [][]string{{"a"}, {"a", "a"}, {"a", "other"}} {
		_, err := ScorePassageComparison(PassageComparison{Baseline: []string{"a", "b"}, Current: good, P1: PassageArm{Ranking: bad}, P3: good}, []Judgment{{DocumentID: "a", Grade: 1}}, 5)
		require.Error(t, err)
	}
}

func TestPairedStagesRotateAndKeepFailuresVisible(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	parentDeadline, _ := parent.Deadline()
	candidates := []PassageCandidate{{DocumentID: "b", Excerpt: "current", Passages: []string{"passage"}, TieBreak: 2}, {DocumentID: "a", Excerpt: "current", Passages: []string{"passage"}, TieBreak: 1}}
	for _, capped := range []bool{true, false} {
		for rotation, order := range [][]string{
			{"provider:current", "select:1", "provider:passage1", "select:3", "provider:passage3"},
			{"select:1", "provider:passage1", "select:3", "provider:passage3", "provider:current"},
			{"select:3", "provider:passage3", "provider:current", "select:1", "provider:passage1"},
		} {
			var calls []string
			stages := map[int]context.Context{}
			done := map[int]<-chan struct{}{}
			selectPassages := func(ctx context.Context, limit int) ([]PassageCandidate, PassageSelection, error) {
				_, ok := ctx.Deadline()
				require.True(t, ok)
				require.NoError(t, ctx.Err())
				stages[limit] = ctx
				// Capture Done while live; canceled contexts may share a closed channel.
				done[limit] = ctx.Done()
				calls = append(calls, fmt.Sprint("select:", limit))
				if capped && limit == 3 {
					return nil, PassageSelection{Capped: true}, errors.New("selection capped")
				}
				selected := slices.Clone(candidates)
				for i := range selected {
					selected[i].Passages = []string{fmt.Sprint("passage", limit)}
				}
				return selected, PassageSelection{}, nil
			}
			runner := &captureEqualRunner{calls: &calls, contexts: map[string]context.Context{}, done: map[string]<-chan struct{}{}}
			comparison, err := EvaluatePassagesWithOptions(parent, "q", candidates, runner, PassageOptions{Deadline: time.Second, Rotation: rotation, Select: selectPassages})
			if capped {
				require.Error(t, err)
				order = slices.DeleteFunc(order, func(call string) bool { return call == "provider:passage3" })
				require.Equal(t, []string{"b", "a"}, comparison.P3.Ranking, "a failed arm retains its baseline")
				require.NotEmpty(t, comparison.P3.Failure)
				require.True(t, comparison.P3.Selection.Capped)
			} else {
				require.NoError(t, err)
				require.Same(t, stages[3], runner.contexts["passage3"])
				require.Equal(t, []string{"a", "b"}, comparison.P3.Ranking)
			}
			require.Equal(t, order, calls)
			require.Same(t, stages[1], runner.contexts["passage1"])
			require.NotEqual(t, done[1], done[3])
			for name, stage := range runner.contexts {
				deadline, ok := stage.Deadline()
				require.True(t, ok, name)
				require.NotEqual(t, parentDeadline, deadline, "each provider must have its fresh stage deadline")
				require.NotEqual(t, parent.Done(), runner.done[name])
				require.ErrorIs(t, stage.Err(), context.Canceled)
			}
			require.NotEqual(t, runner.done["current"], done[1])
			require.NotEqual(t, runner.done["current"], done[3])
			for _, stage := range stages {
				require.ErrorIs(t, stage.Err(), context.Canceled)
			}
			require.Equal(t, []string{"a", "b"}, comparison.Current.Ranking, "ties follow serving identity order")
		}
	}
}

type captureEqualRunner struct {
	calls    *[]string
	contexts map[string]context.Context
	done     map[string]<-chan struct{}
}

func (r *captureEqualRunner) RerankPassages(ctx context.Context, _ string, inputs []PassageInput) (PassageScores, error) {
	*r.calls = append(*r.calls, "provider:"+inputs[0].Text)
	r.contexts[inputs[0].Text], r.done[inputs[0].Text] = ctx, ctx.Done()
	if err := ctx.Err(); err != nil {
		return PassageScores{}, err
	}
	var scores []PassageScore
	for _, input := range inputs {
		scores = append(scores, PassageScore{DocumentID: input.DocumentID, Passage: input.Passage, Score: 1})
	}
	return PassageScores{Scores: scores}, nil
}

func TestPairedProviderFailureRetainsAttemptAccounting(t *testing.T) {
	comparison, err := EvaluatePassages(t.Context(), "q", []PassageCandidate{{DocumentID: "a", Excerpt: "sent text"}}, failedPassageRunner{})
	require.Error(t, err)
	require.Equal(t, 9, comparison.Current.InputBytes)
	require.Equal(t, 31, comparison.Current.RequestBytes)
	require.Equal(t, 4, comparison.Current.LocalTokens)
	require.Equal(t, 1, comparison.Current.Usage.ProviderCalls)
}

type failedPassageRunner struct{}

func (failedPassageRunner) RerankPassages(context.Context, string, []PassageInput) (PassageScores, error) {
	return PassageScores{RequestBytes: 31, LocalTokens: 4, Usage: Usage{ProviderCalls: 1}}, errors.New("synthetic timeout")
}
