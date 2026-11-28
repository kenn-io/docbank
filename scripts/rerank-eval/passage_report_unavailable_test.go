package rerankeval_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
)

func TestPassageIncompleteReportShowsUnavailable(t *testing.T) {
	report := passageGateReport{Observations: []passageObservation{{Corpus: "public", Failure: "preparation failed"}}}
	report.finish()
	require.Equal(t, "inconclusive", report.Summary.Decision)
	require.Zero(t, report.Summary.Queries)
	require.NotContains(t, report.Summary.Reasons, "p1 exceeds a preregistered threshold")
	markdown, err := report.markdown()
	require.NoError(t, err)
	require.Contains(t, markdown, "Decision: **inconclusive**")
	require.Contains(t, markdown, `"public": null`)
	data, err := json.Marshal(report.Summary)
	require.NoError(t, err)
	var result struct {
		Quality map[string]json.RawMessage   `json:"Quality"`
		Arms    []map[string]json.RawMessage `json:"Arms"`
	}
	require.NoError(t, json.Unmarshal(data, &result))
	require.JSONEq(t, "null", string(result.Quality["public"]))
	require.NotEmpty(t, result.Arms)
	require.JSONEq(t, "null", string(result.Arms[0]["MeanBytesRatio"]))
	require.JSONEq(t, "null", string(result.Arms[0]["Full"]))
	require.JSONEq(t, "null", string(result.Arms[0]["Selection"]))
}

func TestPassageIncompleteReportControls(t *testing.T) {
	for _, tc := range []struct {
		name              string
		count             int
		failed, duplicate bool
		available         bool
	}{
		{"partial", 1, false, false, false}, {"failed", 2, true, false, false},
		{"duplicate repeat", 2, false, true, false}, {"complete", 2, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var report passageGateReport
			for q := range tc.count {
				for repeat := range 3 {
					o := passageObservation{Corpus: "short", Query: strconv.Itoa(q), Repeat: repeat}
					if tc.failed && q == 0 && repeat == 0 {
						o.Failure = "failed"
					}
					if tc.duplicate && q == 0 && repeat == 2 {
						o.Repeat = 1
					}
					report.Observations = append(report.Observations, o)
				}
			}
			report.finish()
			data, err := json.Marshal(report.Summary)
			require.NoError(t, err)
			var result struct {
				Quality map[string]json.RawMessage   `json:"Quality"`
				Arms    []map[string]json.RawMessage `json:"Arms"`
			}
			require.NoError(t, json.Unmarshal(data, &result))
			require.NotEmpty(t, result.Arms)
			quality := result.Quality["short"]
			delta := result.Arms[0]["ShortDelta"]
			if tc.available {
				require.NotEmpty(t, quality)
				require.NotEmpty(t, delta)
				require.NotEqual(t, "null", string(quality))
				require.NotEqual(t, "null", string(delta))
			} else {
				require.JSONEq(t, "null", string(quality))
				require.JSONEq(t, "null", string(delta))
			}
		})
	}
}

func TestPassageIncompleteReportDisclosesObservedCap(t *testing.T) {
	report := passageGateReport{Observations: []passageObservation{{
		Corpus: "public", Query: "synthetic",
		Comparison: embeddingeval.PassageComparison{Baseline: []string{"synthetic"}, P1: embeddingeval.PassageArm{Ranking: []string{"synthetic"}, Failure: "selection capped", Selection: embeddingeval.PassageSelection{Capped: true}}},
	}}}
	report.finish()
	require.Equal(t, "inconclusive", report.Summary.Decision)
	data, err := json.Marshal(report.Summary)
	require.NoError(t, err)
	var result struct {
		Arms []map[string]json.RawMessage `json:"Arms"`
	}
	require.NoError(t, json.Unmarshal(data, &result))
	require.Len(t, result.Arms, 2)
	require.JSONEq(t, `{"Capped":true}`, string(result.Arms[0]["Selection"]))
	require.JSONEq(t, "null", string(result.Arms[1]["Selection"]))
	require.JSONEq(t, "null", string(result.Arms[0]["Full"]))
	markdown, err := report.markdown()
	require.NoError(t, err)
	require.Contains(t, markdown, `"Capped": true`)
}
