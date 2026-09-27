package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionSupplementRequiresNewFrozenNumberingSnapshot(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	parentAllocation, err := f.ProductionNumberingForJob(t.Context(), parent.ID)
	require.NoError(t, err)
	child := supplementChildFixtureWith(t, f, parent, 0, "", parentAllocation.SnapshotID)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000047",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	_, err = f.ProductionNumberingForJob(t.Context(), child.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var operations int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_operations
		WHERE operation_id=?`, request.OperationID).Scan(&operations))
	require.Zero(t, operations)
}
