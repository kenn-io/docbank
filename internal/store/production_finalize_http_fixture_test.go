package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

// ProductionFinalizeHTTPFixture has fully reviewed synthetic members and a
// passing retained gate, but leaves finalization to the public command.
func ProductionFinalizeHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string) {
	t.Helper()
	s, first, second, setID, revision, authority := productionDuplicateGateFixture(t)
	require.Equal(t, first.SourceSHA256, second.SourceSHA256)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	return s, filepath.Dir(s.path), setID, revision, draft.ETag, namespace.NamespaceID
}
