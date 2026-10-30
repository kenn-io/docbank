package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
	"unicode/utf8"
)

// PassageCandidate records the actual baseline excerpt and experimentally selected
// passages for one retrieved document, in retrieval order. Selection is the
// caller's responsibility; this type does not establish production equivalence.
type PassageCandidate struct {
	DocumentID string
	Excerpt    string
	Passages   []string
	TieBreak   int // Ascending serving identity order; zero preserves caller order.
}

// PassageInput identifies each text independently within its document.
type PassageInput struct {
	DocumentID string
	Passage    int
	Text       string
}

// PassageScore associates a provider score with its input identity.
type PassageScore struct {
	DocumentID string
	Passage    int
	Score      float64
}

// PassageScores contains the complete provider response and its observed usage.
type PassageScores struct {
	Scores                    []PassageScore
	Usage                     Usage
	RequestBytes, LocalTokens int
}

// PassageRunner executes one provider call for the supplied texts.
type PassageRunner interface {
	RerankPassages(ctx context.Context, query string, inputs []PassageInput) (PassageScores, error)
}

// PassageArm records one reranked order and the provider input and usage.
type PassageArm struct {
	Ranking      []string         `json:"Ranking"`
	Usage        Usage            `json:"Usage"`
	InputBytes   int              `json:"InputBytes"`
	LogicalBytes int              `json:"LogicalBytes"`
	InputRunes   int              `json:"InputRunes"`
	RequestBytes int              `json:"RequestBytes"`
	LocalTokens  int              `json:"LocalTokens"`
	Failure      string           `json:"Failure"`
	Selection    PassageSelection `json:"Selection"`
}

// PassageSelection discloses bounded selection without treating normal top-K
// retrieval as a selection failure.
type PassageSelection struct {
	Capped             bool `json:"Capped"`
	Fallbacks          int  `json:"Fallbacks"`
	Lexical            int  `json:"Lexical"`
	Chunks             int  `json:"Chunks"`
	Excluded           int  `json:"Excluded"`
	UnknownCoordinates int  `json:"UnknownCoordinates"`
	LateSegments       int  `json:"LateSegments"`
}

// PassageOptions keeps selection and execution inside a fresh arm-local stage.
// Select must preserve frozen identities and baseline excerpts.
type PassageOptions struct {
	Deadline time.Duration
	Rotation int
	Select   func(context.Context, int) ([]PassageCandidate, PassageSelection, error)
}

// PassageComparison records a shared baseline and three reranking arms.
type PassageComparison struct {
	Baseline []string   `json:"Baseline"`
	Current  PassageArm `json:"Current"`
	P1       PassageArm `json:"P1"`
	P3       PassageArm `json:"P3"`
}

// EvaluatePassages reranks the same frozen candidate order in three arms.
// Excerpts and passages must already obey the provider's byte limit; silently
// truncating an excerpt here would change the baseline being measured.
func EvaluatePassages(ctx context.Context, query string, candidates []PassageCandidate, runner PassageRunner) (PassageComparison, error) {
	return EvaluatePassagesWithOptions(ctx, query, candidates, runner, PassageOptions{})
}

// EvaluatePassagesWithOptions retains baseline orders on failed arms and still
// attempts the other arms. Callers must disclose its error, never drop a query.
func EvaluatePassagesWithOptions(ctx context.Context, query string, candidates []PassageCandidate, runner PassageRunner, options PassageOptions) (PassageComparison, error) {
	if ctx == nil || query == "" || runner == nil {
		return PassageComparison{}, errors.New("query, context and passage runner are required")
	}
	seen := make(map[string]bool, len(candidates))
	result := PassageComparison{}
	for _, candidate := range candidates {
		if candidate.DocumentID == "" || seen[candidate.DocumentID] {
			return PassageComparison{}, errors.New("candidate IDs must be non-empty and unique")
		}
		seen[candidate.DocumentID] = true
		if !validPassage(candidate.Excerpt) {
			return PassageComparison{}, errors.New("invalid baseline excerpt")
		}
		for _, passage := range candidate.Passages {
			if !validPassage(passage) {
				return PassageComparison{}, errors.New("invalid passage")
			}
		}
		result.Baseline = append(result.Baseline, candidate.DocumentID)
	}
	arms := []*PassageArm{&result.Current, &result.P1, &result.P3}
	limits := []int{0, 1, 3}
	var failures []error
	for offset := range 3 {
		i := ((options.Rotation % 3) + 3 + offset) % 3
		started := time.Now()
		stageCtx, cancel := context.WithCancel(ctx)
		if options.Deadline > 0 {
			cancel()
			stageCtx, cancel = context.WithTimeout(ctx, options.Deadline)
		}
		selected, selection, err := candidates, PassageSelection{}, error(nil)
		if i != 0 && options.Select != nil {
			selected, selection, err = options.Select(stageCtx, limits[i])
			if err == nil && !sameFrozenCandidates(candidates, selected) {
				err = errors.New("selector changed frozen candidates")
			}
		}
		if selection.Capped && err == nil {
			err = errors.New("passage selection capped")
		}
		if err == nil {
			*arms[i], err = runPassageArm(stageCtx, query, selected, limits[i], runner)
		}
		cancel()
		arms[i].Selection = selection
		arms[i].Usage.Latency = time.Since(started)
		if err != nil {
			arms[i].Ranking = slices.Clone(result.Baseline)
			arms[i].Failure = err.Error()
			failures = append(failures, fmt.Errorf("arm %d: %w", limits[i], err))
		}
	}
	return result, errors.Join(failures...)
}

func sameFrozenCandidates(frozen, selected []PassageCandidate) bool {
	if len(frozen) != len(selected) {
		return false
	}
	for i, candidate := range frozen {
		if candidate.DocumentID != selected[i].DocumentID || candidate.Excerpt != selected[i].Excerpt || candidate.TieBreak != selected[i].TieBreak {
			return false
		}
		for _, text := range selected[i].Passages {
			if !validPassage(text) {
				return false
			}
		}
	}
	return true
}

func validPassage(text string) bool {
	return utf8.ValidString(text) && len(text) <= maxRerankExcerptBytes
}

type passageKey struct {
	document string
	passage  int
}

func runPassageArm(ctx context.Context, query string, candidates []PassageCandidate, limit int, runner PassageRunner) (PassageArm, error) {
	if err := ctx.Err(); err != nil {
		return PassageArm{}, err
	}
	arm := PassageArm{}
	inputs := make([]PassageInput, 0, len(candidates))
	for _, candidate := range candidates {
		arm.Ranking = append(arm.Ranking, candidate.DocumentID)
		passages := candidate.Passages
		if limit == 0 || len(passages) == 0 {
			passages = []string{candidate.Excerpt}
		} else {
			passages = passages[:min(limit, len(passages))]
		}
		for i, text := range passages {
			inputs = append(inputs, PassageInput{DocumentID: candidate.DocumentID, Passage: i, Text: text})
			arm.InputBytes += len(text)
			arm.InputRunes += utf8.RuneCountInString(text)
		}
	}
	arm.LogicalBytes = arm.InputBytes + len(query)
	arm.InputRunes += utf8.RuneCountInString(query)
	if len(inputs) == 0 {
		return arm, nil
	}
	// The runner may retain or modify its input; the expected identities remain frozen.
	expected := make(map[passageKey]bool, len(inputs))
	for _, input := range inputs {
		expected[passageKey{input.DocumentID, input.Passage}] = true
	}
	output, err := runner.RerankPassages(ctx, query, slices.Clone(inputs))
	arm.Usage = output.Usage
	arm.RequestBytes, arm.LocalTokens = output.RequestBytes, output.LocalTokens
	if err != nil {
		return arm, err
	}
	if err := ctx.Err(); err != nil {
		return arm, err
	}
	if err := validateUsage(output.Usage); err != nil {
		return arm, err
	}
	if len(output.Scores) != len(expected) {
		return arm, errors.New("invalid passage score count")
	}
	maxima := make(map[string]float64, len(candidates))
	for _, score := range output.Scores {
		key := passageKey{score.DocumentID, score.Passage}
		if !expected[key] || math.IsNaN(score.Score) || math.IsInf(score.Score, 0) {
			return arm, errors.New("invalid or duplicate passage score")
		}
		delete(expected, key)
		if prior, ok := maxima[score.DocumentID]; !ok || score.Score > prior {
			maxima[score.DocumentID] = score.Score
		}
	}
	scores := make([]float64, len(arm.Ranking))
	for i, id := range arm.Ranking {
		scores[i] = maxima[id]
	}
	indices := make([]int, len(candidates))
	for i := range indices {
		indices[i] = i
	}
	slices.SortStableFunc(indices, func(i, j int) int {
		if scores[i] > scores[j] {
			return -1
		}
		if scores[i] < scores[j] {
			return 1
		}
		if candidates[i].TieBreak < candidates[j].TieBreak {
			return -1
		}
		if candidates[i].TieBreak > candidates[j].TieBreak {
			return 1
		}
		return 0
	})
	for i, index := range indices {
		arm.Ranking[i] = candidates[index].DocumentID
	}
	return arm, nil
}

// PassageMetrics records quality metrics for the shared baseline and each arm.
// CriticalMisses uses the caller's cutoff, which must be inside the top 20.
type PassageMetrics struct{ Baseline, Current, P1, P3 MetricSet }

// ScorePassageComparison scores paired orders with a meaningful critical-rank
// cutoff. Judgments may refer to relevant documents absent from retrieval.
func ScorePassageComparison(comparison PassageComparison, judgments []Judgment, cutoff int) (PassageMetrics, error) {
	if cutoff < 1 || cutoff >= 20 || len(judgments) == 0 {
		return PassageMetrics{}, errors.New("judgments and a critical cutoff between 1 and 19 are required")
	}
	seen := make(map[string]bool, len(judgments))
	relevant := false
	for _, judgment := range judgments {
		if judgment.DocumentID == "" || judgment.Grade < 0 || seen[judgment.DocumentID] {
			return PassageMetrics{}, errors.New("invalid or duplicate judgment")
		}
		seen[judgment.DocumentID] = true
		relevant = relevant || judgment.Grade > 0
	}
	if !relevant {
		return PassageMetrics{}, errors.New("at least one relevant judgment is required")
	}
	candidates := make(map[string]Document, len(comparison.Baseline))
	for _, id := range comparison.Baseline {
		candidates[id] = Document{ID: id}
	}
	if err := validateRanking(candidates, comparison.Baseline); err != nil {
		return PassageMetrics{}, err
	}
	for _, arm := range []PassageArm{comparison.Current, comparison.P1, comparison.P3} {
		if len(arm.Ranking) != len(comparison.Baseline) {
			return PassageMetrics{}, errors.New("arm candidate count differs from baseline")
		}
		if err := validateRanking(candidates, arm.Ranking); err != nil {
			return PassageMetrics{}, err
		}
	}
	measure := func(ranking []string) MetricSet {
		metrics := score(judgments, ranking)
		metrics.CriticalMisses = 0
		for _, judgment := range judgments {
			if judgment.Critical && judgment.Grade > 0 {
				index := slices.Index(ranking, judgment.DocumentID)
				if index < 0 || index >= cutoff {
					metrics.CriticalMisses++
				}
			}
		}
		return metrics
	}
	return PassageMetrics{Baseline: measure(comparison.Baseline), Current: measure(comparison.Current.Ranking), P1: measure(comparison.P1.Ranking), P3: measure(comparison.P3.Ranking)}, nil
}
