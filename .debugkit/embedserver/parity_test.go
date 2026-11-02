package embedserver

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/embedtest"
)

// TestNativeParity uses public, synthetic Ollama outputs. Ordinary unit tests
// need no native runtime or network. CI sets both paths and runs this test on
// Linux, macOS, and Windows with cached, checksummed assets.
func TestNativeParity(t *testing.T) {
	dir, library := os.Getenv("KIT_EMBED_TEST_DIR"), os.Getenv("KIT_EMBED_TEST_LIBRARY")
	if dir == "" || library == "" {
		t.Skip("set KIT_EMBED_TEST_DIR and KIT_EMBED_TEST_LIBRARY for native parity")
	}
	samples := embedtest.Load(t, "testdata/ollama.json")
	e, err := loadModel(t.Context(), dir, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	for _, sample := range samples {
		actual, tokens, err := e.embed(t.Context(), sample.Input)
		require.NoError(t, err)
		assert.Equal(t, sample.Tokens, tokens, sample.Input)
		sample.AssertParity(t, actual)
	}
}
