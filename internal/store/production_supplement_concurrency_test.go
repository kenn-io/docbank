package store

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestConcurrentProductionSupplementsKeepDistinctRangesAndReplay(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	first := supplementChildFixture(t, f, parent)
	finalized, err := f.LoadFinalizedProduction(t.Context(), first.SetID, first.Revision)
	require.NoError(t, err)
	second, err := f.AdmitProductionJob(t.Context(), production.JobRequest{
		JobID:       "78000000-0000-4000-8000-000000000020",
		OperationID: "78000000-0000-4000-8000-000000000021",
		SetID:       first.SetID, Revision: first.Revision, ETag: first.ETag,
		PreparedInputSHA256: first.PreparedInputSHA256, RevisionSHA256: first.RevisionSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256,
	})
	require.NoError(t, err)
	requests := [2]production.SupplementRequest{
		{OperationID: "78000000-0000-4000-8000-000000000022", ParentJobID: parent.ID,
			JobID: first.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
			PreparedSHA256: first.RevisionSHA256, PreparedInputSHA256: first.PreparedInputSHA256},
		{OperationID: "78000000-0000-4000-8000-000000000023", ParentJobID: parent.ID,
			JobID: second.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
			PreparedSHA256: second.RevisionSHA256, PreparedInputSHA256: second.PreparedInputSHA256},
	}
	var records [2]production.SupplementRecord
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := range requests {
		wg.Go(func() {
			<-start
			records[index], errs[index] = f.CreateProductionSupplement(t.Context(), "synthetic-operator", requests[index])
		})
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.NotEqual(t, records[0].AllocationID, records[1].AllocationID)
	require.Equal(t, records[0].NamespaceID, records[1].NamespaceID)
	require.True(t, records[0].EndSequence < records[1].StartSequence ||
		records[1].EndSequence < records[0].StartSequence)
	parentAllocation, err := f.ProductionNumberingForJob(t.Context(), parent.ID)
	require.NoError(t, err)
	for index, record := range records {
		require.Greater(t, record.StartSequence, parentAllocation.EndSequence)
		loaded, err := f.LoadProductionSupplement(t.Context(), requests[index].OperationID)
		require.NoError(t, err)
		require.Equal(t, record, loaded)
	}
	// Simulate losing the first response and advancing the child state before
	// the caller retries. Historical operation identity still resolves exactly.
	require.NoError(t, f.CancelProductionJob(t.Context(), first.ID))
	replayed, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", requests[0])
	require.NoError(t, err)
	require.Equal(t, records[0], replayed)
	var allocations int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations
		WHERE operation_id IN (?,?)`, first.ID, second.ID).Scan(&allocations))
	require.Equal(t, 2, allocations)
}
