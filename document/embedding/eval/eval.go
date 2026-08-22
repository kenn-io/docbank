package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Corpus is a versioned, redistributable document retrieval benchmark.
type Corpus struct {
	ID        string
	Version   string
	Documents []Document
	Queries   []Query
}

// Document is one public or synthetic benchmark document.
type Document struct {
	ID   string
	Text string
}

// Query contains query text and graded relevance judgments.
type Query struct {
	ID        string
	Text      string
	Judgments []Judgment
}

// Judgment assigns a non-negative relevance grade to one document. Critical
// judgments identify results whose absence from the top 20 is reported
// separately from aggregate metrics.
type Judgment struct {
	DocumentID string
	Grade      int
	Critical   bool
}

// System identifies one retrieval recipe under evaluation. Fingerprints make
// reports attributable without coupling the evaluator to a provider.
type System struct {
	ID                     string
	RecipeFingerprint      string
	VectorSpaceFingerprint string
}

// Usage records provider work and observed latency for one query.
type Usage struct {
	ProviderCalls       int
	ProviderInputRunes  int
	ProviderOutputUnits int
	EstimatedCostMicros int64
	Latency             time.Duration
}

// SearchResult is one ranked retrieval result.
type SearchResult struct {
	DocumentIDs []string
	Usage       Usage
}

// Runner executes one system/query pair. Hosted runners should return fresh
// observations rather than replaying cached results when repeated trials are
// intended to characterize provider variance.
type Runner interface {
	Search(ctx context.Context, system System, corpus Corpus, query Query) (SearchResult, error)
}

// MetricSet contains the preregistered document retrieval metrics.
type MetricSet struct {
	RecallAt5      float64
	RecallAt10     float64
	RecallAt20     float64
	NDCGAt10       float64
	MRR            float64
	CriticalMisses int
}

// TrialReport contains one complete pass over all benchmark queries.
type TrialReport struct {
	Repetition int
	Metrics    MetricSet
	Usage      Usage
}

// MetricInterval is an empirical repeated-trial range and mean. It is not a
// parametric confidence interval.
type MetricInterval struct {
	Min  float64
	Mean float64
	Max  float64
}

// AggregateReport summarizes repeated hosted or local trials.
type AggregateReport struct {
	RecallAt5      MetricInterval
	RecallAt10     MetricInterval
	RecallAt20     MetricInterval
	NDCGAt10       MetricInterval
	MRR            MetricInterval
	CriticalMisses MetricInterval
}

// SystemReport contains all observations for one retrieval system.
type SystemReport struct {
	System    System
	Trials    []TrialReport
	Aggregate AggregateReport
}

// Report is attributable evaluation output for one corpus version.
type Report struct {
	CorpusID      string
	CorpusVersion string
	Repetitions   int
	Systems       []SystemReport
}

// Evaluate executes every system over every query for the requested number of
// repetitions and computes metrics from observed rankings.
func Evaluate(ctx context.Context, corpus Corpus, systems []System, repetitions int, runner Runner) (Report, error) {
	if err := validateCorpus(corpus); err != nil {
		return Report{}, err
	}
	if len(systems) == 0 || repetitions < 1 || runner == nil {
		return Report{}, errors.New("evaluation requires systems, positive repetitions, and a runner")
	}
	report := Report{CorpusID: corpus.ID, CorpusVersion: corpus.Version, Repetitions: repetitions}
	seenSystems := make(map[string]bool, len(systems))
	for _, system := range systems {
		if system.ID == "" || system.RecipeFingerprint == "" || seenSystems[system.ID] {
			return Report{}, errors.New("evaluation system IDs and recipe fingerprints must be non-empty and unique")
		}
		seenSystems[system.ID] = true
		systemReport := SystemReport{System: system}
		for repetition := range repetitions {
			trial, err := runTrial(ctx, corpus, system, repetition, runner)
			if err != nil {
				return Report{}, fmt.Errorf("evaluate system %q repetition %d: %w", system.ID, repetition+1, err)
			}
			systemReport.Trials = append(systemReport.Trials, trial)
		}
		systemReport.Aggregate = aggregate(systemReport.Trials)
		report.Systems = append(report.Systems, systemReport)
	}
	return report, nil
}

func runTrial(ctx context.Context, corpus Corpus, system System, repetition int, runner Runner) (TrialReport, error) {
	trial := TrialReport{Repetition: repetition + 1}
	for _, query := range corpus.Queries {
		result, err := runner.Search(ctx, system, corpus, query)
		if err != nil {
			return TrialReport{}, fmt.Errorf("query %q: %w", query.ID, err)
		}
		if result.Usage.ProviderCalls < 0 || result.Usage.ProviderInputRunes < 0 || result.Usage.ProviderOutputUnits < 0 ||
			result.Usage.EstimatedCostMicros < 0 || result.Usage.Latency < 0 {
			return TrialReport{}, fmt.Errorf("query %q: provider usage cannot be negative", query.ID)
		}
		if err := validateRanking(corpus, result.DocumentIDs); err != nil {
			return TrialReport{}, fmt.Errorf("query %q: %w", query.ID, err)
		}
		metrics := score(query.Judgments, result.DocumentIDs)
		trial.Metrics.RecallAt5 += metrics.RecallAt5
		trial.Metrics.RecallAt10 += metrics.RecallAt10
		trial.Metrics.RecallAt20 += metrics.RecallAt20
		trial.Metrics.NDCGAt10 += metrics.NDCGAt10
		trial.Metrics.MRR += metrics.MRR
		trial.Metrics.CriticalMisses += metrics.CriticalMisses
		addUsage(&trial.Usage, result.Usage)
	}
	count := float64(len(corpus.Queries))
	trial.Metrics.RecallAt5 /= count
	trial.Metrics.RecallAt10 /= count
	trial.Metrics.RecallAt20 /= count
	trial.Metrics.NDCGAt10 /= count
	trial.Metrics.MRR /= count
	return trial, nil
}

func score(judgments []Judgment, ranking []string) MetricSet {
	grades := make(map[string]int, len(judgments))
	critical := make(map[string]bool)
	relevant := 0
	for _, judgment := range judgments {
		grades[judgment.DocumentID] = judgment.Grade
		if judgment.Grade > 0 {
			relevant++
			if judgment.Critical {
				critical[judgment.DocumentID] = true
			}
		}
	}
	metrics := MetricSet{
		RecallAt5: recallAt(ranking, grades, relevant, 5), RecallAt10: recallAt(ranking, grades, relevant, 10),
		RecallAt20: recallAt(ranking, grades, relevant, 20), NDCGAt10: ndcgAt(ranking, grades, 10),
	}
	for index, documentID := range ranking {
		if grades[documentID] > 0 {
			metrics.MRR = 1 / float64(index+1)
			break
		}
	}
	for documentID := range critical {
		index := slices.Index(ranking, documentID)
		if index < 0 || index >= 20 {
			metrics.CriticalMisses++
		}
	}
	return metrics
}

func recallAt(ranking []string, grades map[string]int, relevant, cutoff int) float64 {
	if relevant == 0 {
		return 1
	}
	found := 0
	for _, documentID := range ranking[:min(len(ranking), cutoff)] {
		if grades[documentID] > 0 {
			found++
		}
	}
	return float64(found) / float64(relevant)
}

func ndcgAt(ranking []string, grades map[string]int, cutoff int) float64 {
	dcg := 0.0
	for index, documentID := range ranking[:min(len(ranking), cutoff)] {
		dcg += discountedGain(grades[documentID], index)
	}
	idealGrades := make([]int, 0, len(grades))
	for _, grade := range grades {
		if grade > 0 {
			idealGrades = append(idealGrades, grade)
		}
	}
	slices.SortFunc(idealGrades, func(left, right int) int { return right - left })
	idcg := 0.0
	for index, grade := range idealGrades[:min(len(idealGrades), cutoff)] {
		idcg += discountedGain(grade, index)
	}
	if idcg == 0 {
		return 1
	}
	return dcg / idcg
}

func discountedGain(grade, zeroBasedRank int) float64 {
	if grade <= 0 {
		return 0
	}
	return (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(zeroBasedRank)+2)
}

func aggregate(trials []TrialReport) AggregateReport {
	return AggregateReport{
		RecallAt5:      interval(trials, func(metric MetricSet) float64 { return metric.RecallAt5 }),
		RecallAt10:     interval(trials, func(metric MetricSet) float64 { return metric.RecallAt10 }),
		RecallAt20:     interval(trials, func(metric MetricSet) float64 { return metric.RecallAt20 }),
		NDCGAt10:       interval(trials, func(metric MetricSet) float64 { return metric.NDCGAt10 }),
		MRR:            interval(trials, func(metric MetricSet) float64 { return metric.MRR }),
		CriticalMisses: interval(trials, func(metric MetricSet) float64 { return float64(metric.CriticalMisses) }),
	}
}

func interval(trials []TrialReport, value func(MetricSet) float64) MetricInterval {
	result := MetricInterval{Min: math.Inf(1), Max: math.Inf(-1)}
	for _, trial := range trials {
		observed := value(trial.Metrics)
		result.Min = min(result.Min, observed)
		result.Max = max(result.Max, observed)
		result.Mean += observed
	}
	result.Mean /= float64(len(trials))
	return result
}

func validateCorpus(corpus Corpus) error {
	if corpus.ID == "" || corpus.Version == "" || len(corpus.Documents) == 0 || len(corpus.Queries) == 0 {
		return errors.New("evaluation corpus ID, version, documents, and queries are required")
	}
	documents := make(map[string]bool, len(corpus.Documents))
	for _, document := range corpus.Documents {
		if document.ID == "" || document.Text == "" || documents[document.ID] {
			return errors.New("evaluation document IDs and text must be non-empty and IDs unique")
		}
		documents[document.ID] = true
	}
	queries := make(map[string]bool, len(corpus.Queries))
	for _, query := range corpus.Queries {
		if query.ID == "" || query.Text == "" || len(query.Judgments) == 0 || queries[query.ID] {
			return errors.New("evaluation query IDs, text, and judgments must be non-empty and IDs unique")
		}
		queries[query.ID] = true
		judged := make(map[string]bool, len(query.Judgments))
		relevant := 0
		for _, judgment := range query.Judgments {
			if !documents[judgment.DocumentID] || judgment.Grade < 0 || judged[judgment.DocumentID] {
				return fmt.Errorf("evaluation query %q has an invalid judgment", query.ID)
			}
			judged[judgment.DocumentID] = true
			if judgment.Grade > 0 {
				relevant++
			}
		}
		if relevant == 0 {
			return fmt.Errorf("evaluation query %q has no relevant judgment", query.ID)
		}
	}
	return nil
}

func validateRanking(corpus Corpus, ranking []string) error {
	documents := make(map[string]bool, len(corpus.Documents))
	for _, document := range corpus.Documents {
		documents[document.ID] = true
	}
	seen := make(map[string]bool, len(ranking))
	for _, documentID := range ranking {
		if !documents[documentID] || seen[documentID] {
			return errors.New("ranking contains an unknown or duplicate document ID")
		}
		seen[documentID] = true
	}
	return nil
}

func addUsage(total *Usage, observed Usage) {
	total.ProviderCalls += observed.ProviderCalls
	total.ProviderInputRunes += observed.ProviderInputRunes
	total.ProviderOutputUnits += observed.ProviderOutputUnits
	total.EstimatedCostMicros += observed.EstimatedCostMicros
	total.Latency += observed.Latency
}
