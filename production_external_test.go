package docbank_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/document/redaction"
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
	_, err = second.ProductionSet(t.Context(), set.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}
