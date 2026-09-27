package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestAppendProductionMembersRejectsReplacementAndReplays(t *testing.T) {
	s, _, set, draft, existing := ProductionReviewHTTPFixture(t)
	added := existing
	added.ID = "89000000-0000-4000-8000-000000000041"
	added.Ordinal = 2
	request := redaction.ApplyRequest{OperationID: "89000000-0000-4000-8000-000000000042", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &added}}}
	receipt, err := s.AppendProductionMembers(t.Context(), "synthetic-operator", set.ID, 1, request)
	require.NoError(t, err)
	require.Equal(t, draft.ETag+1, receipt.ETag)
	replayed, err := s.AppendProductionMembers(t.Context(), "restored-operator", set.ID, 1, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	changed := request
	changed.Changes = []redaction.Change{{Kind: "member", Member: &existing}}
	_, err = s.AppendProductionMembers(t.Context(), "synthetic-operator", set.ID, 1, changed)
	require.ErrorIs(t, err, ErrProductionOperationConflict)
	changed.OperationID = "89000000-0000-4000-8000-000000000043"
	changed.ETag = receipt.ETag
	_, err = s.AppendProductionMembers(t.Context(), "synthetic-operator", set.ID, 1, changed)
	require.ErrorIs(t, err, ErrInvalidProduction)
	changed.Changes = []redaction.Change{{Kind: "member", Member: &added}}
	_, err = s.AppendProductionMembers(t.Context(), "synthetic-operator", set.ID, 1, changed)
	require.ErrorIs(t, err, ErrInvalidProduction)
	duplicate := request
	duplicate.OperationID = "89000000-0000-4000-8000-000000000044"
	duplicate.ETag = receipt.ETag
	duplicate.Changes = []redaction.Change{{Kind: "member", Member: &added}, {Kind: "member", Member: &added}}
	_, err = s.AppendProductionMembers(t.Context(), "synthetic-operator", set.ID, 1, duplicate)
	require.ErrorIs(t, err, ErrInvalidProduction)
	_, err = s.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, 1, request)
	require.ErrorIs(t, err, ErrProductionOperationConflict)
	members, _, err := s.ProductionMembers(t.Context(), set.ID, 1, "", 200)
	require.NoError(t, err)
	require.Len(t, members, 2)
}
