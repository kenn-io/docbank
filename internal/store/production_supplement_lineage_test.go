package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func publishSupplementChild(t *testing.T, f *realRestartFixture, child production.Job) production.Job {
	t.Helper()
	finalized, err := f.LoadFinalizedProduction(t.Context(), child.SetID, child.Revision)
	require.NoError(t, err)
	_, err = f.worker().RunJob(t.Context(), production.JobRequest{
		JobID: child.ID, OperationID: child.OperationID, SetID: child.SetID,
		Revision: child.Revision, ETag: child.ETag,
		PreparedInputSHA256: child.PreparedInputSHA256, RevisionSHA256: child.RevisionSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256,
	})
	require.NoError(t, err)
	published, err := f.LoadProductionJob(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, published.State)
	return published
}

func TestProductionSupplementThreePublishedGenerationsContinueNumbering(t *testing.T) {
	f, first := publishedRealRetentionFixture(t)
	secondChild := supplementChildFixtureWith(t, f, first, 0, "")
	secondRequest := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000010",
		ParentJobID: first.ID, JobID: secondChild.ID, ParentReceiptSHA256: first.Receipt.SHA256,
		PreparedSHA256: secondChild.RevisionSHA256, PreparedInputSHA256: secondChild.PreparedInputSHA256}
	secondLink, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", secondRequest)
	require.NoError(t, err)
	second := publishSupplementChild(t, f, secondChild)
	thirdChild := supplementChildFixtureWith(t, f, second, 30, "")
	thirdRequest := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000040",
		ParentJobID: second.ID, JobID: thirdChild.ID, ParentReceiptSHA256: second.Receipt.SHA256,
		PreparedSHA256: thirdChild.RevisionSHA256, PreparedInputSHA256: thirdChild.PreparedInputSHA256}
	thirdLink, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", thirdRequest)
	require.NoError(t, err)
	third := publishSupplementChild(t, f, thirdChild)
	firstAllocation, err := f.ProductionNumberingForJob(t.Context(), first.ID)
	require.NoError(t, err)
	secondAllocation, err := f.ProductionNumberingForJob(t.Context(), second.ID)
	require.NoError(t, err)
	thirdAllocation, err := f.ProductionNumberingForJob(t.Context(), third.ID)
	require.NoError(t, err)
	require.Equal(t, firstAllocation.NamespaceID, secondAllocation.NamespaceID)
	require.Equal(t, secondAllocation.NamespaceID, thirdAllocation.NamespaceID)
	require.Equal(t, firstAllocation.EndSequence+1, secondAllocation.StartSequence)
	require.Equal(t, secondAllocation.EndSequence+1, thirdAllocation.StartSequence)
	require.Equal(t, first.Receipt.SHA256, secondLink.ParentReceiptSHA256)
	require.Equal(t, second.Receipt.SHA256, thirdLink.ParentReceiptSHA256)
	require.NotEqual(t, second.Receipt.SHA256, third.Receipt.SHA256)
	loaded, err := f.LoadProductionSupplement(t.Context(), thirdRequest.OperationID)
	require.NoError(t, err)
	require.Equal(t, thirdLink, loaded)
}

func TestProductionSupplementRejectsDifferentNumberingNamespace(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	other, err := f.EnsureBatesNamespace(t.Context(), "WRONG", "", 6)
	require.NoError(t, err)
	child := supplementChildFixtureWith(t, f, parent, 0, other.NamespaceID)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000041",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	_, err = f.ProductionNumberingForJob(t.Context(), child.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var operations, audits int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_operations WHERE operation_id=?`,
		request.OperationID).Scan(&operations))
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_audit_evidence WHERE operation_id=?`,
		request.OperationID).Scan(&audits))
	require.Zero(t, operations)
	require.Zero(t, audits)
}
