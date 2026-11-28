package rerankeval_test

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
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
