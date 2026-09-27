package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestFinalizeProductionDraftRequiresCurrentHumanApprovalBeforeAdmission(t *testing.T) {
	s, first, second, _, _, originalAuthority := productionDuplicateGateFixture(t)
	policyValue := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "76000000-0000-4000-8000-000000000001", Version: 1,
		Name: "Synthetic approved production", CreatedAt: "2026-09-27T00:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "metadata-rule", Kind: documentproduction.PolicyRuleScope,
			Predicate: documentproduction.PolicyPredicate{Field: "metadata.title",
				Operator: documentproduction.PolicyOperatorEquals, Values: []string{"Synthetic title"}},
			Disposition: documentproduction.PolicyDispositionProduce,
		}},
		ConflictMode: documentproduction.PolicyConflictReject,
		Approval:     documentproduction.ApprovalRequirement{Required: true},
	}
	preparedPolicy, err := productionservice.PreparePolicyVersion(
		"76000000-0000-4000-8000-000000000002", policyValue)
	require.NoError(t, err)
	policy, err := s.PutProductionPolicy(t.Context(), preparedPolicy)
	require.NoError(t, err)
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "76000000-0000-4000-8000-000000000003",
		Name:        "Synthetic approved set", Instructions: "Produce reviewed synthetic pages.",
		PolicyID: policy.ID, PolicyVersion: policy.Version,
		NumberingRecipeID: redaction.BatesNumberingRecipeID,
	})
	require.NoError(t, err)
	firstDecision := productionAuthorityDecision(first, "76000000-0000-4000-8000-000000000004")
	secondDecision := productionAuthorityDecision(second, "76000000-0000-4000-8000-000000000005")
	_, err = s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "76000000-0000-4000-8000-000000000006", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &first}, {Kind: "member", Member: &second},
			{Kind: "decision", Decision: &firstDecision}, {Kind: "decision", Decision: &secondDecision}},
	})
	require.NoError(t, err)
	current, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	_, err = s.SealProductionMembership(t.Context(), "test-agent", set.ID, draft.Revision,
		redaction.MembershipSealRequest{OperationID: "76000000-0000-4000-8000-000000000007",
			ETag: current.ETag, Total: 2, MemberHash: current.MemberHash})
	require.NoError(t, err)
	for index, memberID := range []string{first.ID, second.ID} {
		stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
		_, err = s.ReviewProductionMember(t.Context(), "test-agent", set.ID, draft.Revision,
			ProductionReviewRequest{OperationID: []string{
				"76000000-0000-4000-8000-000000000008", "76000000-0000-4000-8000-000000000009",
			}[index], ETag: stored.Draft.ETag, MemberID: memberID,
				Binding: productionReviewBindingForTest(t, stored, memberID), Complete: true})
		require.NoError(t, err)
	}
	publicationID := originalAuthority.Prepared.Members[0].EvidencePin.EmailPublicationOperationID
	require.NotEmpty(t, publicationID)
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), set.ID, draft.Revision,
		map[string]string{first.SourceVersionID: publicationID}))
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	current, err = s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	command := ProductionFinalizeCommand{
		SetID: set.ID, Revision: draft.Revision, ETag: current.ETag,
		OperationID: "76000000-0000-4000-8000-000000000010",
		SnapshotID:  "76000000-0000-4000-8000-000000000011",
		NamespaceID: namespace.NamespaceID,
	}
	_, err = s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.ErrorIs(t, err, ErrInvalidProduction)
	var admitted int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		command.OperationID).Scan(&admitted))
	require.Zero(t, admitted, "missing approval cannot admit finalization")

	stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
	subject, err := productionservice.StoredApprovalSubject(stored)
	require.NoError(t, err)
	now := time.Now().UTC()
	grantRecord, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "76000000-0000-4000-8000-000000000012",
		ApprovalID:  "76000000-0000-4000-8000-000000000013",
		Subject:     subject, Evidence: "Synthetic approval evidence.",
	}, stored.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: now.Format(time.RFC3339Nano), EvidenceSHA256: fakeHash("e5"),
		},
	}, now)
	require.NoError(t, err)
	grant, err := s.PutProductionApproval(t.Context(), grantRecord)
	require.NoError(t, err)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), set.ID, draft.Revision,
		ProductionRevisionGateSelection{ApprovalID: grant.ID}))
	finalized, err := s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.NoError(t, err)
	require.Equal(t, "finalized", finalized.Draft.State)
	require.NotEmpty(t, finalized.ReceiptSHA256)
	loaded, err := s.LoadFinalizedProduction(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	require.NotNil(t, loaded.Authority.Prepared.ApprovalEvaluation)
	require.Equal(t, documentproduction.ApprovalStateCurrent,
		loaded.Authority.Prepared.ApprovalEvaluation.State)
	require.Equal(t, grant.SHA256, loaded.Authority.Prepared.ApprovalEvaluation.ApprovalSHA256)
	require.Equal(t, finalized.ReceiptSHA256, loaded.Authority.Receipt.SHA256)
	var allocations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Zero(t, allocations, "finalization does not reserve numbers")
}
