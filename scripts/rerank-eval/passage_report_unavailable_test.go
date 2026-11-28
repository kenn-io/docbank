package rerankeval_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPassageIncompleteReportShowsUnavailable(t *testing.T) {
	report := passageGateReport{Observations: []passageObservation{{Corpus: "public", Failure: "preparation failed"}}}
	report.finish()
	require.Equal(t, "inconclusive", report.Summary.Decision)
	require.Zero(t, report.Summary.Queries)
	require.NotContains(t, report.Summary.Reasons, "p1 exceeds a preregistered threshold")
	data, err := json.Marshal(report.Summary)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result))
	require.Nil(t, result["Quality"].(map[string]any)["public"])
	arm := result["Arms"].([]any)[0].(map[string]any)
	require.Nil(t, arm["MeanBytesRatio"])
	require.Nil(t, arm["Full"])
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
					o := passageObservation{Corpus: "short", Query: fmt.Sprint(q), Repeat: repeat}
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
			var result map[string]any
			require.NoError(t, json.Unmarshal(data, &result))
			quality := result["Quality"].(map[string]any)["short"]
			delta := result["Arms"].([]any)[0].(map[string]any)["ShortDelta"]
			if tc.available {
				require.NotNil(t, quality)
				require.NotNil(t, delta)
			} else {
				require.Nil(t, quality)
				require.Nil(t, delta)
			}
		})
	}
}
