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
	_, err = second.ProductionSet(t.Context(), set.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}
