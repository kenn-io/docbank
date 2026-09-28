package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedWithheldSelectionBindsSealedMembership(t *testing.T) {
	metadata, root, set, draft, member := store.ProductionReviewHTTPFixture(t)
	_, err := metadata.SealProductionMembership(t.Context(), "synthetic-operator", set.ID, 1,
		api.ProductionMembershipSealRequest{
			OperationID: "87878787-8787-4787-8787-878787878711",
			Total:       1, MemberHash: draft.MemberHash,
		}.Domain(draft.ETag))
	require.NoError(t, err)
	require.NoError(t, metadata.Close())
	embedded, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	request := docbank.ProductionWithheldSelectionCreateRequest{
		OperationID:  "87878787-8787-4787-8787-878787878712",
		SelectionID:  "87878787-8787-4787-8787-878787878713",
		PolicySHA256: draft.Policy.PolicySHA256,
		Members: []documentproduction.WithheldMember{{
			ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
			SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
			FamilyOrder: 1, Family: member.Family,
		}},
	}
	created, err := embedded.CreateProductionWithheldSelection(t.Context(), set.ID, 1, request)
	require.NoError(t, err)
	require.NoError(t, documentproduction.ValidateWithheldSelection(created))
	replay, err := embedded.CreateProductionWithheldSelection(t.Context(), set.ID, 1, request)
	require.NoError(t, err)
	require.Equal(t, created, replay)
	changed := request
	changed.Members = append([]documentproduction.WithheldMember(nil), request.Members...)
	changed.Members[0].SourceSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = embedded.CreateProductionWithheldSelection(t.Context(), set.ID, 1, changed)
	require.Error(t, err)
	separate, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, separate.Close()) })
	_, err = separate.CreateProductionWithheldSelection(t.Context(), set.ID, 1, request)
	require.ErrorIs(t, err, store.ErrNotFound)
}
