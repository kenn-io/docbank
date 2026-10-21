package scanfixture

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScanEvidenceBaseline(t *testing.T) {
	const maxSourceBytes = 128 << 20
	var rows []Measurement
	for _, fixture := range Corpus() {
		measurement := Measure(fixture, maxSourceBytes)
		if fixture.Name == "malformed" {
			// Pin our stable error prefix while normalizing pdfcpu-owned suffix wording.
			for _, message := range []*string{&measurement.PageError, &measurement.InspectError, &measurement.MetadataError} {
				require.True(t, strings.HasPrefix(*message, "validate PDF object graph:"), "unexpected malformed-PDF error: %s", *message)
				*message = "validate PDF object graph: <dependency error>"
			}
		}
		rows = append(rows, measurement)
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
