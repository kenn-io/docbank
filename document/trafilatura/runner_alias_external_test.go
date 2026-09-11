package trafilatura_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/isolate"
	"go.kenn.io/docbank/document/trafilatura"
)

type legacyRunner struct{}

func (legacyRunner) Identity() string {
	return "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
}
func (legacyRunner) Run(context.Context, trafilatura.IsolatedRunRequest) (trafilatura.IsolatedRunResult, error) {
	return trafilatura.IsolatedRunResult{Attestation: trafilatura.IsolationAttestation{}}, trafilatura.ErrIsolationUnavailable
}

func TestLegacyRunnerAliasesAndSentinels(t *testing.T) {
	var runner isolate.IsolatedRunner = legacyRunner{}
	_, err := runner.Run(t.Context(), isolate.IsolatedRunRequest{Requirements: trafilatura.IsolationRequirements{}})
	require.ErrorIs(t, err, isolate.ErrIsolationUnavailable)
	assert.Equal(t, isolate.ErrIsolationUnavailable, trafilatura.ErrIsolationUnavailable)
	assert.Equal(t, isolate.ErrChildFailed, trafilatura.ErrChildFailed)
	assert.Equal(t, isolate.ErrChildOutputTooLarge, trafilatura.ErrChildOutputTooLarge)
	assert.Equal(t, isolate.MaxExecutableBytes, trafilatura.MaxExecutableBytes)
}
