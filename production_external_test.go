package docbank_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedProductionSetsKeepVaultRootsSeparate(t *testing.T) {
	first, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	request := redaction.CreateRequest{OperationID: "88888888-8888-4888-8888-888888888888", Name: "Synthetic review"}
	set, draft, err := first.CreateProductionSet(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, set.ID, draft.SetID)
	catalog, err := first.ProductionRecipes(t.Context())
	require.NoError(t, err)
	require.Equal(t, redaction.DefaultRecipeID, catalog.DefaultID)
	require.Len(t, catalog.Items, 2)
	require.Equal(t, draft.RecipeSHA256, catalog.Items[0].SHA256)
	replaySet, replayDraft, err := first.CreateProductionSet(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, set, replaySet)
	require.Equal(t, draft, replayDraft)
	read, err := first.ProductionSet(t.Context(), set.ID)
	require.NoError(t, err)
	require.Equal(t, set, read)
	page, err := first.ProductionMembers(t.Context(), set.ID, 1, "", 1)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	edit := api.ProductionInstructionsRequest{OperationID: "88888888-8888-4888-8888-888888888889",
		Instructions: "Review synthetic pages"}
	receipt, err := first.EditProductionInstructions(t.Context(), "synthetic-operator", set.ID, 1, 1, edit)
	require.NoError(t, err)
	require.EqualValues(t, 2, receipt.ETag)
	replayed, err := first.EditProductionInstructions(t.Context(), "synthetic-operator", set.ID, 1, 1, edit)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = first.EditProductionInstructions(t.Context(), "synthetic-operator", set.ID, 1, 1,
		api.ProductionInstructionsRequest{OperationID: "88888888-8888-4888-8888-888888888890"})
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	change, err := first.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, 1, 2,
		api.ProductionChangesRequest{OperationID: "88888888-8888-4888-8888-888888888891",
			Changes: []api.ProductionChange{{Kind: "recipe", RecipeID: redaction.RecipeID600DPI}}})
	require.NoError(t, err)
	require.EqualValues(t, 3, change.ETag)
	updated, err := first.ProductionDraft(t.Context(), set.ID, 1)
	require.NoError(t, err)
	require.Equal(t, redaction.RecipeID600DPI, updated.RecipeID)
	forked, err := first.ForkProductionDraft(t.Context(), "synthetic-operator", set.ID, 1,
		"88888888-8888-4888-8888-888888888896")
	require.NoError(t, err)
	require.EqualValues(t, 2, forked.Revision)
	require.Equal(t, updated.RecipeSHA256, forked.RecipeSHA256)
	replayFork, err := first.ForkProductionDraft(t.Context(), "synthetic-operator", set.ID, 1,
		"88888888-8888-4888-8888-888888888896")
	require.NoError(t, err)
	require.Equal(t, forked, replayFork)
	_, err = second.ProductionDraft(t.Context(), set.ID, 2)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.SealProductionMembership(t.Context(), "synthetic-operator", set.ID, 1, 3,
		api.ProductionMembershipSealRequest{OperationID: "88888888-8888-4888-8888-888888888893",
			Total: 1, MemberHash: updated.MemberHash})
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	_, err = first.ReviewProductionMember(t.Context(), "synthetic-operator", set.ID, 1, 3,
		"88888888-8888-4888-8888-888888888894", api.ProductionMemberReviewRequest{
			OperationID: "88888888-8888-4888-8888-888888888895",
			Binding:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Complete: true})
	require.ErrorIs(t, err, store.ErrInvalidProduction)
	_, err = second.ProductionSet(t.Context(), set.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}
