package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
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
	Reranker               string
	RerankerFingerprint    string
	RerankTopN             int
}

// CostObservation records a caller-priced provider cost. Basis identifies the
// dated rate or unit price used to calculate Micros.
type CostObservation struct {
	Micros int64  `json:"micros"`
	Basis  string `json:"basis"`
}

// Usage records provider work and observed latency for one query.
type Usage struct {
	ProviderCalls       int
	ProviderInputRunes  int
	ProviderOutputUnits int
	EstimatedCostMicros int64
	Latency             time.Duration
	TokenUsage          *float64
	Cost                *CostObservation
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
	HitAt1         float64
	HitAt10        float64
	CriticalMisses int
}

// QueryReport records the final ranking and stage observations for one query.
type QueryReport struct {
	QueryID     string
	Ranking     []string
	Metrics     MetricSet
	Usage       Usage
	RerankUsage Usage
}

// TrialReport contains one complete pass over all benchmark queries.
type TrialReport struct {
	Repetition  int
	Metrics     MetricSet
	Usage       Usage
	RerankUsage Usage
	Queries     []QueryReport
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
	HitAt1         MetricInterval
	HitAt10        MetricInterval
	CriticalMisses MetricInterval
}

// PerformanceReport summarizes per-query operational observations. Nil token
// and cost values mean that at least one contributing query lacked evidence.
type PerformanceReport struct {
	QueryCount             int
	P95Latency             time.Duration
	RequestsPerQuery       float64
	RerankRequestsPerQuery float64
	TokensPerQuery         *float64
	RerankTokensPerQuery   *float64
	CostPerQuery           *CostObservation
}

// SystemReport contains all observations for one retrieval system.
type SystemReport struct {
	System      System
	Trials      []TrialReport
	Aggregate   AggregateReport
	Performance PerformanceReport
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
	documentIDs, err := validateCorpus(corpus)
	if err != nil {
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
		reranker, err := validateReranker(system, runner)
		if err != nil {
			return Report{}, err
		}
		seenSystems[system.ID] = true
		systemReport := SystemReport{System: system}
		for repetition := range repetitions {
			trial, err := runTrial(ctx, corpus, documentIDs, system, repetition, runner, reranker)
			if err != nil {
				return Report{}, fmt.Errorf("evaluate system %q repetition %d: %w", system.ID, repetition+1, err)
			}
			systemReport.Trials = append(systemReport.Trials, trial)
		}
		systemReport.Aggregate = aggregate(systemReport.Trials)
		systemReport.Performance = performance(systemReport.Trials)
		report.Systems = append(report.Systems, systemReport)
	}
	return report, nil
}

func runTrial(
	ctx context.Context,
	corpus Corpus,
	documentIDs map[string]Document,
	system System,
	repetition int,
	runner Runner,
	reranker RerankingRunner,
) (TrialReport, error) {
	trial := TrialReport{Repetition: repetition + 1}
	for _, query := range corpus.Queries {
		if system.Reranker != "" && ctx != nil {
			if err := ctx.Err(); err != nil {
				return TrialReport{}, err
			}
		}
		result, err := runner.Search(ctx, system, corpus, query)
		if err != nil {
			return TrialReport{}, fmt.Errorf("query %q: %w", query.ID, err)
		}
		if err := validateUsage(result.Usage); err != nil {
			return TrialReport{}, fmt.Errorf("query %q: %w", query.ID, err)
		}
		if err := validateRanking(documentIDs, result.DocumentIDs); err != nil {
			return TrialReport{}, fmt.Errorf("query %q: %w", query.ID, err)
		}
		if system.Reranker != "" && ctx != nil {
			if err := ctx.Err(); err != nil {
				return TrialReport{}, err
			}
		}

		ranking := slices.Clone(result.DocumentIDs)
		rerankUsage := zeroUsage("no-rerank-stage")
		if system.Reranker != "" {
			if len(ranking) == 0 {
				rerankUsage = zeroUsage("no-rerank-candidates")
			} else {
				prefixCount := min(system.RerankTopN, len(ranking))
				candidates := make([]Document, prefixCount)
				for index, documentID := range ranking[:prefixCount] {
					document := documentIDs[documentID]
					document.Text = boundedExcerpt(document.Text)
					candidates[index] = document
				}
				result, err := reranker.Rerank(ctx, system, query.Text, slices.Clone(candidates))
				if err != nil {
					return TrialReport{}, fmt.Errorf("query %q rerank: %w", query.ID, err)
				}
				if ctx != nil {
					if err := ctx.Err(); err != nil {
						return TrialReport{}, err
					}
				}
				if err := validateRerankResult(result, prefixCount); err != nil {
					return TrialReport{}, fmt.Errorf("query %q: %w", query.ID, err)
				}
				rerankUsage = result.Usage
				ranking = reorderRanking(ranking, result.Scores, prefixCount)
			}
		}
		totalUsage := resultUsage(result, rerankUsage, system.Reranker != "")
		metrics := score(query.Judgments, ranking)
		trial.Metrics.RecallAt5 += metrics.RecallAt5
		trial.Metrics.RecallAt10 += metrics.RecallAt10
		trial.Metrics.RecallAt20 += metrics.RecallAt20
		trial.Metrics.NDCGAt10 += metrics.NDCGAt10
		trial.Metrics.MRR += metrics.MRR
		trial.Metrics.HitAt1 += metrics.HitAt1
		trial.Metrics.HitAt10 += metrics.HitAt10
		trial.Metrics.CriticalMisses += metrics.CriticalMisses
		trial.Queries = append(trial.Queries, QueryReport{
			QueryID: query.ID, Ranking: slices.Clone(ranking), Metrics: metrics,
			Usage: totalUsage, RerankUsage: rerankUsage,
		})
	}
	count := float64(len(corpus.Queries))
	trial.Metrics.RecallAt5 /= count
	trial.Metrics.RecallAt10 /= count
	trial.Metrics.RecallAt20 /= count
	trial.Metrics.NDCGAt10 /= count
	trial.Metrics.MRR /= count
	trial.Metrics.HitAt1 /= count
	trial.Metrics.HitAt10 /= count
	for index, query := range trial.Queries {
		if index == 0 {
			trial.Usage = query.Usage
			trial.RerankUsage = query.RerankUsage
			continue
		}
		trial.Usage = sumUsage(trial.Usage, query.Usage)
		trial.RerankUsage = sumUsage(trial.RerankUsage, query.RerankUsage)
	}
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
		HitAt1: hitAt(ranking, grades, 1), HitAt10: hitAt(ranking, grades, 10),
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

func hitAt(ranking []string, grades map[string]int, cutoff int) float64 {
	for _, documentID := range ranking[:min(len(ranking), cutoff)] {
		if grades[documentID] > 0 {
			return 1
		}
	}
	return 0
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
		HitAt1:         interval(trials, func(metric MetricSet) float64 { return metric.HitAt1 }),
		HitAt10:        interval(trials, func(metric MetricSet) float64 { return metric.HitAt10 }),
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

func validateCorpus(corpus Corpus) (map[string]Document, error) {
	if corpus.ID == "" || corpus.Version == "" || len(corpus.Documents) == 0 || len(corpus.Queries) == 0 {
		return nil, errors.New("evaluation corpus ID, version, documents, and queries are required")
	}
	documents := make(map[string]Document, len(corpus.Documents))
	for _, document := range corpus.Documents {
		if document.ID == "" || document.Text == "" || documents[document.ID].ID != "" {
			return nil, errors.New("evaluation document IDs and text must be non-empty and IDs unique")
		}
		documents[document.ID] = document
	}
	queries := make(map[string]bool, len(corpus.Queries))
	for _, query := range corpus.Queries {
		if query.ID == "" || query.Text == "" || len(query.Judgments) == 0 || queries[query.ID] {
			return nil, errors.New("evaluation query IDs, text, and judgments must be non-empty and IDs unique")
		}
		queries[query.ID] = true
		judged := make(map[string]bool, len(query.Judgments))
		relevant := 0
		for _, judgment := range query.Judgments {
			if documents[judgment.DocumentID].ID == "" || judgment.Grade < 0 || judged[judgment.DocumentID] {
				return nil, fmt.Errorf("evaluation query %q has an invalid judgment", query.ID)
			}
			judged[judgment.DocumentID] = true
			if judgment.Grade > 0 {
				relevant++
			}
		}
		if relevant == 0 {
			return nil, fmt.Errorf("evaluation query %q has no relevant judgment", query.ID)
		}
	}
	return documents, nil
}

func validateRanking(documents map[string]Document, ranking []string) error {
	seen := make(map[string]bool, len(ranking))
	for _, documentID := range ranking {
		if documents[documentID].ID == "" || seen[documentID] {
			return errors.New("ranking contains an unknown or duplicate document ID")
		}
		seen[documentID] = true
	}
	return nil
}

func validateReranker(system System, runner Runner) (RerankingRunner, error) {
	if system.Reranker == "" {
		return nil, nil //nolint:nilnil // nil reranker denotes a disabled stage
	}
	if system.RerankerFingerprint == "" || system.RerankTopN < 1 {
		return nil, errors.New("reranker fingerprint and positive top-N are required")
	}
	reranker, ok := runner.(RerankingRunner)
	if !ok {
		return nil, errors.New("runner does not support the requested reranker")
	}
	return reranker, nil
}

func validateUsage(usage Usage) error {
	if usage.ProviderCalls < 0 || usage.ProviderInputRunes < 0 || usage.ProviderOutputUnits < 0 ||
		usage.EstimatedCostMicros < 0 || usage.Latency < 0 {
		return errors.New("provider usage cannot be negative")
	}
	if usage.TokenUsage != nil && (*usage.TokenUsage < 0 || math.IsNaN(*usage.TokenUsage) || math.IsInf(*usage.TokenUsage, 0)) {
		return errors.New("provider token usage must be finite and non-negative")
	}
	if usage.Cost != nil && (usage.Cost.Micros < 0 || strings.TrimSpace(usage.Cost.Basis) == "") {
		return errors.New("provider cost must have a non-negative value and basis")
	}
	return nil
}

func validateRerankResult(result RerankResult, candidateCount int) error {
	if err := validateUsage(result.Usage); err != nil {
		return err
	}
	if len(result.Scores) != candidateCount {
		return errors.New("reranker returned an invalid score count")
	}
	for _, score := range result.Scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return errors.New("reranker returned a non-finite score")
		}
	}
	return nil
}

func resultUsage(result SearchResult, rerank Usage, hasReranker bool) Usage {
	if !hasReranker {
		return result.Usage
	}
	return sumUsage(result.Usage, rerank)
}

func zeroUsage(basis string) Usage {
	zero := float64(0)
	return Usage{TokenUsage: &zero, Cost: &CostObservation{Basis: basis}}
}

func sumUsage(left, right Usage) Usage {
	result := Usage{
		ProviderCalls:       left.ProviderCalls + right.ProviderCalls,
		ProviderInputRunes:  left.ProviderInputRunes + right.ProviderInputRunes,
		ProviderOutputUnits: left.ProviderOutputUnits + right.ProviderOutputUnits,
		EstimatedCostMicros: left.EstimatedCostMicros + right.EstimatedCostMicros,
		Latency:             left.Latency + right.Latency,
	}
	if left.TokenUsage != nil && right.TokenUsage != nil {
		value := *left.TokenUsage + *right.TokenUsage
		result.TokenUsage = &value
	}
	if left.Cost != nil && right.Cost != nil {
		result.Cost = &CostObservation{Micros: left.Cost.Micros + right.Cost.Micros,
			Basis: combineBasis(left.Cost.Basis, right.Cost.Basis)}
	}
	return result
}

func combineBasis(left, right string) string {
	parts := make(map[string]struct{})
	for _, basis := range []string{left, right} {
		for part := range strings.SplitSeq(basis, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				parts[part] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(parts))
	for part := range parts {
		ordered = append(ordered, part)
	}
	slices.Sort(ordered)
	return strings.Join(ordered, ",")
}

func performance(trials []TrialReport) PerformanceReport {
	result := PerformanceReport{}
	var latencies []time.Duration
	var totalRequests, totalRerankRequests int
	var totalTokens, totalRerankTokens float64
	var totalCost int64
	basis := ""
	tokensKnown, rerankTokensKnown, costKnown := true, true, true
	for _, trial := range trials {
		for _, query := range trial.Queries {
			result.QueryCount++
			latencies = append(latencies, query.Usage.Latency)
			totalRequests += query.Usage.ProviderCalls
			totalRerankRequests += query.RerankUsage.ProviderCalls
			if query.Usage.TokenUsage == nil {
				tokensKnown = false
			} else {
				totalTokens += *query.Usage.TokenUsage
			}
			if query.RerankUsage.TokenUsage == nil {
				rerankTokensKnown = false
			} else {
				totalRerankTokens += *query.RerankUsage.TokenUsage
			}
			if query.Usage.Cost == nil {
				costKnown = false
			} else {
				totalCost += query.Usage.Cost.Micros
				basis = combineBasis(basis, query.Usage.Cost.Basis)
			}
		}
	}
	if result.QueryCount == 0 {
		return result
	}
	result.RequestsPerQuery = float64(totalRequests) / float64(result.QueryCount)
	result.RerankRequestsPerQuery = float64(totalRerankRequests) / float64(result.QueryCount)
	if tokensKnown {
		value := totalTokens / float64(result.QueryCount)
		result.TokensPerQuery = &value
	}
	if rerankTokensKnown {
		value := totalRerankTokens / float64(result.QueryCount)
		result.RerankTokensPerQuery = &value
	}
	if costKnown {
		result.CostPerQuery = &CostObservation{
			Micros: int64(math.Round(float64(totalCost) / float64(result.QueryCount))), Basis: basis,
		}
	}
	slices.Sort(latencies)
	rank := int(math.Ceil(0.95 * float64(len(latencies))))
	rank = max(rank, 1)
	result.P95Latency = latencies[rank-1]
	return result
}
