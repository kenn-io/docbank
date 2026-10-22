package scanfixture

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScanEvidenceBaseline(t *testing.T) {
	const maxSourceBytes = 128 << 20
	var rows []Measurement
	for _, fixture := range Corpus() {
		rows = append(rows, Measure(fixture, maxSourceBytes))
	}
	encoded, err := json.Marshal(struct {
		MaxSourceBytes int64
		Measurements   []Measurement
	}{MaxSourceBytes: maxSourceBytes, Measurements: rows},
		json.Deterministic(true), jsontext.WithIndent("  "))
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	const path = "testdata/measurements.golden.json"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(path, encoded, 0o644))
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(raw), string(encoded))
}
