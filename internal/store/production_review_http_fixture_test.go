package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

// ProductionReviewHTTPFixture builds one synthetic draft with a retained
// source map and decision, ready for the public seal and review routes.
func ProductionReviewHTTPFixture(t *testing.T) (*Store, string, redaction.Set, redaction.Draft, redaction.Member) {
	t.Helper()
	s, member := seedProductionGateAuthority(t)
	member.ID, member.Ordinal = "89000000-0000-4000-8000-000000000001", 1
	set, draft, err := s.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "89000000-0000-4000-8000-000000000002", Name: "Synthetic review"})
	require.NoError(t, err)
	decision := productionAuthorityDecision(member, "89000000-0000-4000-8000-000000000003")
	_, err = s.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, 1, redaction.ApplyRequest{
		OperationID: "89000000-0000-4000-8000-000000000004", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &member}, {Kind: "decision", Decision: &decision}},
	})
	require.NoError(t, err)
	draft, err = s.ProductionDraft(t.Context(), set.ID, 1)
	require.NoError(t, err)
	return s, filepath.Dir(s.path), set, draft, member
}

func ProductionReviewBindingHTTPFixture(t *testing.T, s *Store, setID, memberID string) string {
	t.Helper()
	return productionReviewBindingForTest(t, loadProductionInputsForTest(t, s, setID, 1), memberID)
}
