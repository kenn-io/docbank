package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionSupplementReadRejectsDetachedFinalizedSnapshot(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000049",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	link, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	loaded, err := f.LoadProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, link, loaded)
	finalized, err := f.LoadFinalizedProduction(t.Context(), child.SetID, child.Revision)
	require.NoError(t, err)
	otherSnapshot, err := f.SealProductionNumberingSnapshot(t.Context(),
		"78000000-0000-4000-8000-000000000048", finalized.Authority.Audit.OperationID)
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `UPDATE bates_allocations SET snapshot_id=?
		WHERE allocation_id=?`, otherSnapshot.SnapshotID, link.AllocationID)
	require.ErrorContains(t, err, "immutable")
	_, err = f.db.ExecContext(t.Context(), `DROP TRIGGER bates_allocations_transition_guard`)
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `UPDATE bates_allocations SET snapshot_id=?
		WHERE allocation_id=?`, otherSnapshot.SnapshotID, link.AllocationID)
	require.NoError(t, err)
	_, err = f.LoadProductionSupplement(t.Context(), request.OperationID)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
}

func TestProductionSupplementReadRejectsParentSetDrift(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000050",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	_, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	other, _, err := f.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "78000000-0000-4000-8000-000000000051",
		Name:        "Synthetic unrelated set", Instructions: "Synthetic review instructions",
	})
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `UPDATE production_jobs SET set_id=? WHERE job_id=?`,
		other.ID, parent.ID)
	require.ErrorContains(t, err, "immutable")
	_, err = f.db.ExecContext(t.Context(), `DROP TRIGGER production_jobs_immutable_identity`)
	require.NoError(t, err)
	conn, err := f.db.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE production_jobs SET set_id=? WHERE job_id=?`,
		other.ID, parent.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	_, err = f.LoadProductionSupplement(t.Context(), request.OperationID)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
}
