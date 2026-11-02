// Package embedtest shares the Ollama reference fixture and numerical comparison.
package embedtest

import (
	json "encoding/json/v2"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sample is one reference embedding and token count from Ollama.
type Sample struct {
	Input  string    `json:"input"`
	Tokens int       `json:"tokens"`
	Vector []float32 `json:"vector"`
}

// Load reads the fixture from an explicit path relative to the calling package.
func Load(t *testing.T, path string) []Sample {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var fixture struct {
		Samples []Sample `json:"samples"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.NotEmpty(t, fixture.Samples)
	return fixture.Samples
}

// AssertParity compares an embedding to the reference at cosine 0.999 or better.
func (s Sample) AssertParity(t *testing.T, actual []float32) {
	t.Helper()
	require.Len(t, s.Vector, 768)
	require.Len(t, actual, len(s.Vector))
	var dot, left, right float64
	for i, value := range actual {
		x, y := float64(value), float64(s.Vector[i])
		dot += x * y
		left += x * x
		right += y * y
	}
	assert.GreaterOrEqual(t, dot/math.Sqrt(left*right), 0.999, s.Input)
}
