package store

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/production"
)

func supplementChildFixture(t *testing.T, f *realRestartFixture, parent production.Job) production.Job {
	t.Helper()
	const forkID = "78000000-0000-4000-8000-000000000001"
	fork, err := f.ForkProductionDraft(t.Context(), "synthetic-operator", parent.SetID, parent.Revision, forkID)
	require.NoError(t, err)
	_, err = f.SealProductionMembership(t.Context(), "synthetic-operator", fork.SetID, fork.Revision,
		redaction.MembershipSealRequest{OperationID: "78000000-0000-4000-8000-000000000002",
			ETag: fork.ETag, Total: 2, MemberHash: fork.MemberHash})
	require.NoError(t, err)
	members, _, err := f.ProductionMembers(t.Context(), fork.SetID, fork.Revision, "", 200)
	require.NoError(t, err)
	require.Len(t, members, 2)
	for index, member := range members {
		stored := loadProductionInputsForTest(t, f.Store, fork.SetID, fork.Revision)
		_, err = f.ReviewProductionMember(t.Context(), "synthetic-operator", fork.SetID, fork.Revision,
			ProductionReviewRequest{OperationID: []string{
				"78000000-0000-4000-8000-000000000003",
				"78000000-0000-4000-8000-000000000004",
			}[index], ETag: stored.Draft.ETag, MemberID: member.ID,
				Binding: productionReviewBindingForTest(t, stored, member.ID), Complete: true})
		require.NoError(t, err)
	}
	parentFinalized, err := f.LoadFinalizedProduction(t.Context(), parent.SetID, parent.Revision)
	require.NoError(t, err)
	emailOperations := make(map[string]string)
	for _, prepared := range parentFinalized.Authority.Prepared.Members {
		if prepared.EvidencePin.EmailPublicationOperationID != "" {
			emailOperations[prepared.Member.SourceVersionID] = prepared.EvidencePin.EmailPublicationOperationID
		}
	}
	require.NoError(t, f.PinProductionRevisionEvidence(t.Context(), fork.SetID, fork.Revision, emailOperations))
	require.NoError(t, f.SelectProductionRevisionGateAuthority(t.Context(), fork.SetID, fork.Revision,
		ProductionRevisionGateSelection{}))
	stored := loadProductionInputsForTest(t, f.Store, fork.SetID, fork.Revision)
	revisionSHA, err := production.ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(f.Store, f.LoadProductionGateSnapshot)
	require.NoError(t, err)
	authority, err := production.RunPreparedInputGates(t.Context(), gateStore,
		production.PreparedInputRequest{OperationID: "78000000-0000-4000-8000-000000000005",
			ReceiptID: "78000000-0000-4000-8000-000000000006", SetID: fork.SetID,
			Revision: fork.Revision, ExpectedETag: stored.Draft.ETag,
			ExpectedRevisionSHA256: revisionSHA, PreparedAt: time.Now().UTC()})
	require.NoErrorf(t, err, "gate findings: %#v", authority.GateResults)
	require.NotNil(t, authority.Receipt)
	const snapshotID = "78000000-0000-4000-8000-000000000007"
	_, err = f.SealProductionNumberingSnapshot(t.Context(), snapshotID, authority.Audit.OperationID)
	require.NoError(t, err)
	parentAllocation, err := f.ProductionNumberingForJob(t.Context(), parent.ID)
	require.NoError(t, err)
	draft := stored.Draft
	draft.State = "finalized"
	require.NoError(t, f.FinalizeProductionRevision(t.Context(), production.FinalizationRequest{
		Finalized:   production.FinalizedProduction{Draft: draft, Authority: authority},
		OperationID: authority.Audit.OperationID, NamespaceID: parentAllocation.NamespaceID,
		SnapshotID: snapshotID, RecipeSHA256: draft.NumberingRecipeSHA256,
	}))
	child, err := f.AdmitProductionJob(t.Context(), production.JobRequest{
		JobID:       "78000000-0000-4000-8000-000000000008",
		OperationID: "78000000-0000-4000-8000-000000000009",
		SetID:       draft.SetID, Revision: draft.Revision, ETag: draft.ETag,
		PreparedInputSHA256: authority.Receipt.SHA256, RevisionSHA256: authority.Prepared.SHA256,
		NumberingProfileSHA256: draft.NumberingRecipeSHA256,
	})
	require.NoError(t, err)
	require.NotEqual(t, parent.RevisionSHA256, child.RevisionSHA256)
	return child
}

func TestProductionSupplementSurvivesMetadataRestore(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000013",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	record, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	var metadata bytes.Buffer
	require.NoError(t, f.ExportMetadata(t.Context(), &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	loaded, err := restored.LoadProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, record, loaded)
	allocation, err := restored.ProductionNumberingForJob(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, record.AllocationID, allocation.AllocationID)
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	require.NotEmpty(t, roundTrip.Bytes())
}

func TestCreateProductionSupplementRejectsDriftAndRollsBackLateFailure(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000011",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	parentAllocation, err := f.ProductionNumberingForJob(t.Context(), parent.ID)
	require.NoError(t, err)
	var initialCursor int64
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT next_sequence FROM bates_namespace_cursors WHERE namespace_id=?`,
		parentAllocation.NamespaceID).Scan(&initialCursor))
	wrong := request
	wrong.ParentReceiptSHA256 = child.PreparedInputSHA256
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", wrong)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	wrong = request
	wrong.PreparedSHA256 = parent.RevisionSHA256
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", wrong)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	wrong = request
	wrong.ParentJobID = "78000000-0000-4000-8000-000000000012"
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", wrong)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	_, err = f.db.ExecContext(t.Context(), `CREATE TRIGGER supplement_test_abort BEFORE INSERT ON production_operations
		WHEN NEW.kind='supplement' BEGIN SELECT RAISE(ABORT, 'synthetic late failure'); END`)
	require.NoError(t, err)
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
	_, err = f.ProductionNumberingForJob(t.Context(), child.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var cursor int64
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT next_sequence FROM bates_namespace_cursors WHERE namespace_id=?`,
		parentAllocation.NamespaceID).Scan(&cursor))
	require.Equal(t, initialCursor, cursor)
	_, err = f.db.ExecContext(t.Context(), `DROP TRIGGER supplement_test_abort`)
	require.NoError(t, err)
	record, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, initialCursor, record.StartSequence)
	wrong = request
	wrong.PreparedInputSHA256 = parent.PreparedInputSHA256
	_, err = f.CreateProductionSupplement(t.Context(), "synthetic-operator", wrong)
	require.ErrorIs(t, err, production.ErrSupplementConflict)
}

func TestCreateProductionSupplementCommitsParentAndContinuationTogether(t *testing.T) {
	f, parent := publishedRealRetentionFixture(t)
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000010",
		ParentJobID: parent.ID, JobID: child.ID,
		ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256:      child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	record, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.NoError(t, production.ValidateSupplementRecord(record))
	require.Equal(t, parent.ID, record.ParentJobID)
	require.Equal(t, child.ID, record.JobID)
	parentAllocation, err := f.ProductionNumberingForJob(t.Context(), parent.ID)
	require.NoError(t, err)
	childAllocation, err := f.ProductionNumberingForJob(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, parentAllocation.EndSequence+1, childAllocation.StartSequence)
	require.Equal(t, childAllocation.AllocationID, record.AllocationID)
	replayed, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, record, replayed)
	loaded, err := f.LoadProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, record, loaded)
	var receipts, operations, audits int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`, request.OperationID).Scan(&receipts))
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_operations WHERE operation_id=?`, request.OperationID).Scan(&operations))
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM production_audit_evidence WHERE operation_id=?`, request.OperationID).Scan(&audits))
	require.Equal(t, 1, receipts)
	require.Equal(t, 1, operations)
	require.Equal(t, 1, audits)
}
