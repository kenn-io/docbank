package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedProductionSupplementUsesPublishedAuthority(t *testing.T) {
	metadata, root, request := store.PublishedProductionSupplementHTTPFixture(t)
	require.NoError(t, metadata.Close())
	embedded, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, embedded.Close()) })

	record, err := embedded.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.NoError(t, production.ValidateSupplementRecord(record))
	require.Equal(t, request.OperationID, record.OperationID)
	require.Equal(t, request.ParentJobID, record.ParentJobID)
	require.Greater(t, record.StartSequence, record.ParentEndSequence)
	loaded, err := embedded.ProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, record, loaded)

	replay, err := embedded.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, record, replay)
	changed := request
	changed.ParentReceiptSHA256 = request.PreparedSHA256
	_, err = embedded.CreateProductionSupplement(t.Context(), "synthetic-operator", changed)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	loaded, err = embedded.ProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, record, loaded)

	separate, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, separate.Close()) })
	_, err = separate.ProductionSupplement(t.Context(), request.OperationID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = separate.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.Error(t, err)
}
