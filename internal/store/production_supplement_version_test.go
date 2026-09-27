package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionSupplementKeepsOldVersionAndRejectsStaleNewReservation(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	parentFinalized, err := f.LoadFinalizedProduction(t.Context(), parent.SetID, parent.Revision)
	require.NoError(t, err)
	oldSource := parentFinalized.Authority.Prepared.Members[0].Member
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000042",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	link, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	publishedChild := publishSupplementChild(t, f, child)
	childAllocation, err := f.ProductionNumberingForJob(t.Context(), child.ID)
	require.NoError(t, err)
	oldNumber, err := f.FindPublishedProductionNumber(t.Context(), childAllocation.Labels[0].Label)
	require.NoError(t, err)
	require.Equal(t, oldSource.SourceVersionID, oldNumber.SourceVersionID)
	require.Equal(t, publishedChild.ID, oldNumber.JobID)

	replacement, err := f.write(t.Context(), bytes.NewReader([]byte("synthetic corrected source")))
	require.NoError(t, err)
	liveNode, err := f.NodeByID(t.Context(), oldSource.NodeID)
	require.NoError(t, err)
	_, newVersion, err := f.ReplaceContent(t.Context(), liveNode.ID, liveNode.Revision,
		replacement.Hash, replacement.Size, liveNode.MimeType)
	require.NoError(t, err)
	require.NotEqual(t, oldSource.SourceVersionID, newVersion.ID)
	require.Equal(t, oldSource.NodeID, newVersion.NodeID)
	stillOld, err := f.FindPublishedProductionNumber(t.Context(), childAllocation.Labels[0].Label)
	require.NoError(t, err)
	require.Equal(t, oldNumber, stillOld)
	loaded, err := f.LoadProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, link, loaded)
	replayed, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, link, replayed)

	finalizedChild, err := f.LoadProductionPackageInputs(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, oldSource.SourceVersionID, finalizedChild.Members[0].SourceVersionID)
	stale, err := f.AdmitProductionJob(t.Context(), production.JobRequest{
		JobID:       "78000000-0000-4000-8000-000000000043",
		OperationID: "78000000-0000-4000-8000-000000000044",
		SetID:       child.SetID, Revision: child.Revision, ETag: child.ETag,
		PreparedInputSHA256: child.PreparedInputSHA256, RevisionSHA256: child.RevisionSHA256,
		NumberingProfileSHA256: parentFinalized.Draft.NumberingRecipeSHA256,
	})
	require.NoError(t, err)
	var before, after int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&before))
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", production.SupplementRequest{
		OperationID: "78000000-0000-4000-8000-000000000045",
		ParentJobID: parent.ID, JobID: stale.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: stale.RevisionSHA256, PreparedInputSHA256: stale.PreparedInputSHA256,
	})
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&after))
	require.Equal(t, before, after)

	var metadata bytes.Buffer
	require.NoError(t, f.ExportMetadata(t.Context(), &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	restoredLink, err := restored.LoadProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, link, restoredLink)
	restoredNumber, err := restored.FindPublishedProductionNumber(t.Context(), childAllocation.Labels[0].Label)
	require.NoError(t, err)
	require.Equal(t, oldNumber, restoredNumber)
}
