package rerankeval_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
)

type passageObservation struct {
	Corpus     string                          `json:"Corpus"`
	Query      string                          `json:"Query"`
	Repeat     int                             `json:"Repeat"`
	Proxy      bool                            `json:"Proxy"`
	Comparison embeddingeval.PassageComparison `json:"Comparison"`
	Metrics    embeddingeval.PassageMetrics    `json:"-"`
	NDCGAt10   [4]float64                      `json:"NDCGAt10"`
	HitAt1     [4]float64                      `json:"HitAt1"`
	Failure    string                          `json:"Failure"`
}
type passageGateSummary struct {
	Decision                 string                       `json:"Decision"`
	Reasons                  []string                     `json:"Reasons"`
	Queries                  int                          `json:"Queries"`
	ProxyQueries             int                          `json:"ProxyQueries"`
	Observations             int                          `json:"Observations"`
	Failures                 int                          `json:"Failures"`
	CriticalEvidence         string                       `json:"CriticalEvidence"`
	Quality                  map[string][4]passageQuality `json:"Quality"`
	RetrievedPositiveQueries int                          `json:"RetrievedPositiveQueries"`
	Arms                     [2]passageGateArm            `json:"Arms"`
	Preparation              time.Duration                `json:"Preparation"`
	Budget                   passageBudget                `json:"Budget"`
}
type passageQuality struct {
	Name     string  `json:"Name"`
	NDCGAt10 float64 `json:"NDCGAt10"`
	HitAt1   float64 `json:"HitAt1"`
}
type passageGateArm struct {
	Name             string                         `json:"Name"`
	Full             [2]float64                     `json:"Full"`
	Proxy            [2]float64                     `json:"Proxy"`
	ShortDelta       float64                        `json:"ShortDelta"`
	SyntheticDelta   float64                        `json:"SyntheticDelta"`
	MeanBytesRatio   float64                        `json:"MeanBytesRatio"`
	MeanTokensRatio  float64                        `json:"MeanTokensRatio"`
	P95BytesRatio    float64                        `json:"P95BytesRatio"`
	WorstBytesRatio  float64                        `json:"WorstBytesRatio"`
	P95TokensRatio   float64                        `json:"P95TokensRatio"`
	WorstTokensRatio float64                        `json:"WorstTokensRatio"`
	MeanJSONBytes    float64                        `json:"MeanJSONBytes"`
	P95Latency       time.Duration                  `json:"P95Latency"`
	P3MinusP1        float64                        `json:"P3MinusP1"`
	HitAt1           float64                        `json:"HitAt1"`
	Selection        embeddingeval.PassageSelection `json:"Selection"`
}
type passageGateReport struct {
	Summary      passageGateSummary           `json:"Summary"`
	Observations []passageObservation         `json:"Observations"`
	Versions     map[string]map[string]string `json:"Versions"`
}

func passageInterval(values []float64) [2]float64 {
	if len(values) == 0 {
		return [2]float64{}
	}
	//nolint:gosec // G404: the preregistered bootstrap seed must be reproducible.
	rng := rand.New(rand.NewPCG(20261030, 20261030))
	samples := make([]float64, 10000)
	for i := range samples {
		for range values {
			samples[i] += values[rng.IntN(len(values))]
		}
		samples[i] /= float64(len(values))
	}
	slices.Sort(samples)
	return [2]float64{samples[249], samples[9749]}
}
func passageMean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(max(1, len(values)))
}
func passageQuantile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v := slices.Clone(values)
	slices.Sort(v)
	return v[int(math.Ceil(fraction*float64(len(v))))-1]
}
func passageRatio(a, b float64) float64 {
	if b <= 0 {
		return math.MaxFloat64
	}
	return a / b
}
func (r *passageGateReport) finish() {
	s := &r.Summary
	s.Decision = "pass"
	s.CriticalEvidence = "unavailable"
	s.Observations = len(r.Observations)
	grouped := map[string][]passageObservation{}
	controls := map[string]int{}
	for _, o := range r.Observations {
		key := o.Corpus + "\x00" + o.Query
		if len(grouped[key]) == 0 {
			if o.Corpus == "public" {
				s.Queries++
				if o.Metrics.Baseline.RecallAt20 > 0 {
					s.RetrievedPositiveQueries++
				}
				if o.Proxy {
					s.ProxyQueries++
				}
			} else {
				controls[o.Corpus]++
			}
		}
		grouped[key] = append(grouped[key], o)
		if o.Failure != "" {
			s.Failures++
		}
	}
	complete := s.Failures == 0 && s.Queries == 100 && s.ProxyQueries >= 30 && controls["short"] == 2 && controls["synthetic"] == 4 && s.Budget.EstimatedMicros <= 25_000_000
	keys := slices.Sorted(maps.Keys(grouped))
	s.Quality = map[string][4]passageQuality{}
	for _, corpus := range []string{"public", "proxy", "short", "synthetic"} {
		var quality [4]passageQuality
		for arm, name := range []string{"no-rerank", "current", "p1", "p3"} {
			var ndcg, hits []float64
			for _, key := range keys {
				rows := grouped[key]
				if rows[0].Corpus != corpus && (corpus != "proxy" || rows[0].Corpus != "public" || !rows[0].Proxy) {
					continue
				}
				var ndcgSum, hitSum float64
				for _, o := range rows {
					metrics := [4]embeddingeval.MetricSet{o.Metrics.Baseline, o.Metrics.Current, o.Metrics.P1, o.Metrics.P3}
					ndcgSum += metrics[arm].NDCGAt10
					hitSum += metrics[arm].HitAt1
				}
				ndcg = append(ndcg, ndcgSum/float64(len(rows)))
				hits = append(hits, hitSum/float64(len(rows)))
			}
			quality[arm] = passageQuality{Name: name, NDCGAt10: passageMean(ndcg), HitAt1: passageMean(hits)}
		}
		s.Quality[corpus] = quality
	}
	for arm := range 2 {
		a := &s.Arms[arm]
		a.Name = []string{"p1", "p3"}[arm]
		diffs := map[string][]float64{}
		var bytes, tokens, jsonBytes, baseBytes, baseTokens, byteRatios, tokenRatios, latency, currentLatency, hit, p3p1 []float64
		for _, key := range keys {
			rows := grouped[key]
			if len(rows) != 3 {
				complete = false
			}
			var deltas []float64
			for _, o := range rows {
				arms := []embeddingeval.PassageArm{o.Comparison.P1, o.Comparison.P3}
				metrics := []embeddingeval.MetricSet{o.Metrics.P1, o.Metrics.P3}
				v := arms[arm]
				m := metrics[arm]
				a.Selection.Capped = a.Selection.Capped || v.Selection.Capped
				deltas = append(deltas, m.NDCGAt10-o.Metrics.Current.NDCGAt10)
				if v.Failure != "" || v.Selection.Capped || o.Comparison.Current.Failure != "" {
					complete = false
				}
				if o.Corpus != "public" {
					continue
				}
				bytes = append(bytes, float64(v.LogicalBytes))
				tokens = append(tokens, float64(v.LocalTokens))
				jsonBytes = append(jsonBytes, float64(v.RequestBytes))
				baseBytes = append(baseBytes, float64(o.Comparison.Current.LogicalBytes))
				baseTokens = append(baseTokens, float64(o.Comparison.Current.LocalTokens))
				byteRatios = append(byteRatios, passageRatio(float64(v.LogicalBytes), float64(o.Comparison.Current.LogicalBytes)))
				tokenRatios = append(tokenRatios, passageRatio(float64(v.LocalTokens), float64(o.Comparison.Current.LocalTokens)))
				latency = append(latency, float64(v.Usage.Latency))
				currentLatency = append(currentLatency, float64(o.Comparison.Current.Usage.Latency))
				hit = append(hit, m.HitAt1)
				p3p1 = append(p3p1, o.Metrics.P3.NDCGAt10-o.Metrics.P1.NDCGAt10)
				d := &a.Selection
				x := v.Selection
				d.Fallbacks += x.Fallbacks
				d.Lexical += x.Lexical
				d.Chunks += x.Chunks
				d.Excluded += x.Excluded
				d.UnknownCoordinates += x.UnknownCoordinates
				d.LateSegments += x.LateSegments
			}
			corpus := rows[0].Corpus
			diffs[corpus] = append(diffs[corpus], passageMean(deltas))
			if corpus == "public" && rows[0].Proxy {
				diffs["proxy"] = append(diffs["proxy"], passageMean(deltas))
			}
		}
		a.Full, a.Proxy = passageInterval(diffs["public"]), passageInterval(diffs["proxy"])
		a.ShortDelta, a.SyntheticDelta = passageMean(diffs["short"]), passageMean(diffs["synthetic"])
		a.MeanBytesRatio = passageRatio(passageMean(bytes), passageMean(baseBytes))
		a.MeanTokensRatio = passageRatio(passageMean(tokens), passageMean(baseTokens))
		a.MeanJSONBytes = passageMean(jsonBytes)
		a.P95BytesRatio, a.P95TokensRatio = passageQuantile(byteRatios, .95), passageQuantile(tokenRatios, .95)
		a.WorstBytesRatio, a.WorstTokensRatio = passageQuantile(byteRatios, 1), passageQuantile(tokenRatios, 1)
		a.P95Latency = time.Duration(passageQuantile(latency, .95))
		a.HitAt1, a.P3MinusP1 = passageMean(hit), passageMean(p3p1)
		if a.Proxy[0] <= 0 || a.Full[0] < -0.01 || a.Full[1] < 0 || a.ShortDelta < -0.01 || a.SyntheticDelta < -0.01 || a.P95Latency > 5*time.Second || float64(a.P95Latency) > 1.5*passageQuantile(currentLatency, .95) || a.MeanBytesRatio > 4 || a.MeanTokensRatio > 4 {
			s.Decision = "fail"
			s.Reasons = append(s.Reasons, a.Name+" exceeds a preregistered threshold")
		}
	}
	if !complete {
		s.Decision = "inconclusive"
		s.Reasons = append(s.Reasons, "missing observations, provider/selection failures, controls or proxy coverage")
	}
}
func (r *passageGateReport) markdown() (string, error) {
	data, err := json.MarshalIndent(r.Summary, "", "  ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# Passage rerank gate 2026-10-30v1\n\nDecision: **%s**. MLDR whole-positive visibility proxy; critical-span evidence: unavailable.\n\nThe no-rerank arm sends no reranker request and incurs zero reranker usage. The local cl100k_base estimate is independent of Voyage tokens. Embedding token usage is unavailable from the existing provider contract; the budget reserves a local-token cost estimate for one attempt per embedding invocation.\n\n```json\n%s\n```\n", r.Summary.Decision, data), nil
}
