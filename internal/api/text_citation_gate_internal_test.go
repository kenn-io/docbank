package api

import (
	"github.com/stretchr/testify/require"
	"testing"
)

// BeginTextCitationTestFreeze exposes only the real backup freeze to external tests.
func BeginTextCitationTestFreeze(t *testing.T, gate *OperationGate) func() {
	t.Helper()
	freezer := &gateFreezer{gate: gate}
	require.NoError(t, freezer.Begin(t.Context()))
	return func() { require.NoError(t, freezer.End(t.Context())) }
}
