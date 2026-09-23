package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionSnapshotManifestLimitRejectsBeforeWrite(t *testing.T) {
	s := newTestStore(t)
	const snapshotID = "75000000-0000-4000-8000-000000000040"
	request := SnapshotSealRequest{SnapshotID: snapshotID, Members: []CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: "75000000-0000-4000-8000-000000000041",
		NodeID: 1, ContentVersionID: "75000000-0000-4000-8000-000000000042",
		BlobSHA256: productionHash("source"), Size: 1,
		FamilyID: "75000000-0000-4000-8000-000000000041", FamilyOrder: 1,
		DisplayName: "Synthetic occurrence", FrozenFieldsJSON: strings.Repeat("x", 2048),
		DocumentKind: "production",
	}}}
	_, err := s.sealCollectionSnapshot(t.Context(), request, true, 1024,
		func(context.Context, *sql.Tx, SnapshotSealRequest) error {
			t.Fatal("oversize manifest reached storage validation")
			return nil
		})
	require.ErrorIs(t, err, ErrPackageConflict)
	var retained int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM collection_snapshots WHERE snapshot_id=?`, snapshotID).Scan(&retained))
	require.Zero(t, retained)
}

func TestProductionStoredGateFinalizesDuplicateOccurrencesAndReservesOnce(t *testing.T) {
	s, first, second, setID, revision, authority := productionDuplicateGateFixture(t)
	const snapshotID = "75000000-0000-4000-8000-000000000020"
	snapshot, err := s.SealProductionNumberingSnapshot(t.Context(), snapshotID, authority.Audit.OperationID)
	require.NoError(t, err)
	require.Equal(t, 2, snapshot.MemberCount)
	require.Equal(t, 2, snapshot.PageCount)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	draft.State = "finalized"
	finalized := production.FinalizedProduction{Draft: draft, Authority: authority}
	finalization := production.FinalizationRequest{
		Finalized: finalized, OperationID: authority.Audit.OperationID,
		NamespaceID: namespace.NamespaceID, SnapshotID: snapshotID,
		RecipeSHA256: draft.NumberingRecipeSHA256,
	}
	require.NoError(t, s.FinalizeProductionRevision(t.Context(), finalization))
	loaded, err := s.LoadFinalizedProduction(t.Context(), setID, revision)
	require.NoError(t, err)
	require.Equal(t, authority.Receipt.SHA256, loaded.Authority.Receipt.SHA256)
	require.Equal(t, authority.Prepared.SHA256, loaded.Authority.Prepared.SHA256)
	productionEvidenceMetadata(t, s, first.SourceSHA256, fakeHash("94"), "Later synthetic title")
	require.NoError(t, s.FinalizeProductionRevision(t.Context(), finalization), "a byte-identical replay remains historical")
	_, err = s.EditProductionInstructions(t.Context(), "test-agent", setID, revision, redaction.InstructionsEditRequest{
		OperationID: "75000000-0000-4000-8000-000000000026", ETag: draft.ETag,
		Instructions: "A finalized revision cannot be edited.",
	})
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
	job, err := s.AdmitProductionJob(t.Context(), production.JobRequest{
		JobID:       "75000000-0000-4000-8000-000000000021",
		OperationID: "75000000-0000-4000-8000-000000000022",
		SetID:       setID, Revision: revision, ETag: draft.ETag,
		PreparedInputSHA256:    authority.Receipt.SHA256,
		RevisionSHA256:         authority.Prepared.SHA256,
		NumberingProfileSHA256: draft.NumberingRecipeSHA256,
	})
	require.NoError(t, err)
	wrongReceipt := job
	wrongReceipt.PreparedInputSHA256 = productionHash("wrong gate receipt")
	_, err = s.ReserveProductionJobNumbers(t.Context(), wrongReceipt, loaded)
	require.Error(t, err)
	var allocationsBefore int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations`).Scan(&allocationsBefore))
	require.Zero(t, allocationsBefore)
	reserved, err := s.ReserveProductionJobNumbers(t.Context(), job, loaded)
	require.NoError(t, err)
	require.Equal(t, []documentproduction.AssignedNumber{
		{MemberID: first.ID, MemberOrdinal: 1, Page: 1, Text: "PROD000001"},
		{MemberID: second.ID, MemberOrdinal: 2, Page: 1, Text: "PROD000002"},
	}, reserved.Numbers)
	replay, err := s.ReserveProductionJobNumbers(t.Context(), job, loaded)
	require.NoError(t, err)
	require.Equal(t, reserved, replay)
	allocation, err := s.ProductionNumberingForJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, reserved.ID, allocation.AllocationID)
}

func TestProductionFinalizationRejectsChangedRevisionAndSnapshotBeforeNumbering(t *testing.T) {
	for _, change := range []string{"revision", "snapshot"} {
		t.Run(change, func(t *testing.T) {
			s, _, _, setID, revision, authority := productionDuplicateGateFixture(t)
			const snapshotID = "75000000-0000-4000-8000-000000000030"
			_, err := s.SealProductionNumberingSnapshot(t.Context(), snapshotID, authority.Audit.OperationID)
			require.NoError(t, err)
			namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
			require.NoError(t, err)
			draft, err := s.ProductionDraft(t.Context(), setID, revision)
			require.NoError(t, err)
			if change == "revision" {
				_, err = s.EditProductionInstructions(t.Context(), "test-agent", setID, revision,
					redaction.InstructionsEditRequest{
						OperationID: "75000000-0000-4000-8000-000000000031", ETag: draft.ETag,
						Instructions: "Changed after the gate was recorded.",
					})
				require.NoError(t, err)
			}
			draft.State = "finalized"
			request := production.FinalizationRequest{
				Finalized:   production.FinalizedProduction{Draft: draft, Authority: authority},
				OperationID: authority.Audit.OperationID,
				NamespaceID: namespace.NamespaceID, SnapshotID: snapshotID,
				RecipeSHA256: draft.NumberingRecipeSHA256,
			}
			if change == "snapshot" {
				other, _ := batesFixture(t, s)
				request.SnapshotID = other.SnapshotID
			}
			require.Error(t, s.FinalizeProductionRevision(t.Context(), request))
			var finalized, allocations int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_finalized_revisions WHERE set_id=? AND revision=?`,
				setID, revision).Scan(&finalized))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
			require.Zero(t, finalized)
			require.Zero(t, allocations)
		})
	}
}

func productionDuplicateGateFixture(t *testing.T) (*Store, redaction.Member, redaction.Member, string, int64, documentproduction.PreparedInputAuthority) {
	t.Helper()
	s, source := seedProductionGateAuthority(t)
	first, second := source, source
	first.ID, first.Ordinal = "75000000-0000-4000-8000-000000000010", 1
	second.ID, second.Ordinal = "75000000-0000-4000-8000-000000000011", 2
	first.Family.Kind, second.Family.Kind = "email_message", "email_message"
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "75000000-0000-4000-8000-000000000012", Name: "Synthetic duplicate production",
		Instructions:      "Keep the selected synthetic character.",
		NumberingRecipeID: redaction.BatesNumberingRecipeID,
	})
	require.NoError(t, err)
	firstDecision := productionAuthorityDecision(first, "75000000-0000-4000-8000-000000000013")
	secondDecision := productionAuthorityDecision(second, "75000000-0000-4000-8000-000000000014")
	_, err = s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "75000000-0000-4000-8000-000000000015", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &first}, {Kind: "member", Member: &second},
			{Kind: "decision", Decision: &firstDecision}, {Kind: "decision", Decision: &secondDecision}},
	})
	require.NoError(t, err)
	current, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	_, err = s.SealProductionMembership(t.Context(), "test-agent", set.ID, draft.Revision, redaction.MembershipSealRequest{
		OperationID: "75000000-0000-4000-8000-000000000016", ETag: current.ETag,
		Total: 2, MemberHash: current.MemberHash,
	})
	require.NoError(t, err)
	for index, memberID := range []string{first.ID, second.ID} {
		stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
		_, err = s.ReviewProductionMember(t.Context(), "test-agent", set.ID, draft.Revision, ProductionReviewRequest{
			OperationID: []string{"75000000-0000-4000-8000-000000000017", "75000000-0000-4000-8000-000000000018"}[index],
			ETag:        stored.Draft.ETag, MemberID: memberID,
			Binding: productionReviewBindingForTest(t, stored, memberID), Complete: true,
		})
		require.NoError(t, err)
	}
	productionEvidenceMetadata(t, s, source.SourceSHA256, fakeHash("93"), "Synthetic title")
	view, err := s.EmailMetadata(t.Context(), source.SourceVersionID)
	require.NoError(t, err)
	emailOperationID := "75000000-0000-4000-8000-000000000024"
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, emailOperationID))
	require.NoError(t, err)
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view,
		"75000000-0000-4000-8000-000000000025"))
	require.NoError(t, err)
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), set.ID, draft.Revision,
		map[string]string{source.SourceVersionID: emailOperationID}))
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), set.ID, draft.Revision,
		ProductionRevisionGateSelection{}))
	stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
	revisionSHA, err := production.ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	require.NoError(t, err)
	authority, err := production.RunPreparedInputGates(t.Context(), gateStore, production.PreparedInputRequest{
		OperationID: "75000000-0000-4000-8000-000000000019",
		ReceiptID:   "75000000-0000-4000-8000-000000000023",
		SetID:       set.ID, Revision: draft.Revision, ExpectedETag: stored.Draft.ETag,
		ExpectedRevisionSHA256: revisionSHA,
		PreparedAt:             time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
	})
	require.NoErrorf(t, err, "gate results: %#v", authority.GateResults)
	require.NotNil(t, authority.Receipt)
	require.Len(t, authority.Prepared.Members, 2)
	for _, member := range authority.Prepared.Members {
		require.Equal(t, source.SourceVersionID, member.Member.SourceVersionID)
		require.Equal(t, emailOperationID, member.EvidencePin.EmailPublicationOperationID)
		require.NotEmpty(t, member.EvidencePin.PolicyFactsSHA256)
	}
	return s, first, second, set.ID, draft.Revision, authority
}

func TestProductionFinalizationRejectsChangedPinnedEvidenceBeforeNumbering(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevision(t, "metadata.title")
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("91"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	revisionSHA256, err := production.ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	require.NoError(t, err)
	request := production.PreparedInputRequest{
		OperationID: "75000000-0000-4000-8000-000000000001",
		ReceiptID:   "75000000-0000-4000-8000-000000000002",
		SetID:       setID, Revision: revision, ExpectedETag: stored.Draft.ETag,
		ExpectedRevisionSHA256: revisionSHA256,
		PreparedAt:             time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC),
	}
	authority, err := production.RunPreparedInputGates(t.Context(), gateStore, request)
	require.NoError(t, err)
	require.NotNil(t, authority.Receipt)

	// A later metadata generation leaves the old pin and receipt retained, but
	// makes that exact observation stale for a new finalization.
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("92"), "Changed synthetic title")
	finalizedDraft := stored.Draft
	finalizedDraft.State = "finalized"
	err = s.FinalizeProductionRevision(t.Context(), production.FinalizationRequest{
		Finalized:    production.FinalizedProduction{Draft: finalizedDraft, Authority: authority},
		OperationID:  request.OperationID,
		NamespaceID:  "75000000-0000-4000-8000-000000000003",
		SnapshotID:   "75000000-0000-4000-8000-000000000004",
		RecipeSHA256: productionHash("numbering recipe"),
	})
	require.Error(t, err)
	var finalized, allocations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_finalized_revisions WHERE set_id=? AND revision=?`, setID, revision).Scan(&finalized))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Zero(t, finalized)
	require.Zero(t, allocations)
}
