package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

func productionEvidenceRevision(t *testing.T, policyField string) (*Store, redaction.Member, string, int64) {
	t.Helper()
	return productionEvidenceRevisionWithPolicy(t, policyField, "standalone", false, false)
}

func productionEmailEvidenceRevision(t *testing.T) (*Store, redaction.Member, string, int64) {
	t.Helper()
	return productionEvidenceRevisionWithPolicy(t, "", "email_message", false, false)
}

func productionEvidenceRevisionWithPolicy(t *testing.T, policyField, familyKind string, approvalRequired, privilegeRequired bool) (*Store, redaction.Member, string, int64) {
	t.Helper()
	s, member := seedProductionGateAuthority(t)
	member.ID, member.Ordinal = "74000000-0000-4000-8000-000000000001", 1
	member.Family.Kind = familyKind
	request := redaction.CreateRequest{
		OperationID: "74000000-0000-4000-8000-000000000002", Name: "Synthetic evidence pins",
		Instructions: "Use retained synthetic evidence only.",
	}
	if policyField != "" || approvalRequired || privilegeRequired {
		disposition := documentproduction.PolicyDispositionProduce
		if privilegeRequired {
			disposition = documentproduction.PolicyDispositionWithhold
		}
		privilege := documentproduction.PrivilegeLogRequirement{}
		if privilegeRequired {
			privilege = documentproduction.PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
				RequiredFields: []string{"date"}, AllowedBases: []string{"synthetic_basis"}}
		}
		policyValue := documentproduction.PolicyVersion{
			Contract: documentproduction.PolicyContractV1,
			ID:       "74000000-0000-4000-8000-000000000003", Version: 1,
			Name: "Synthetic metadata rule", CreatedAt: "2026-09-23T00:00:00Z",
			Rules: []documentproduction.PolicyRule{{
				ID: "metadata-rule", Kind: documentproduction.PolicyRuleScope,
				Predicate: documentproduction.PolicyPredicate{Field: policyField,
					Operator: documentproduction.PolicyOperatorEquals, Values: []string{"Synthetic title"}},
				Disposition: disposition,
			}}, ConflictMode: documentproduction.PolicyConflictReject,
			Approval:     documentproduction.ApprovalRequirement{Required: approvalRequired},
			PrivilegeLog: privilege,
		}
		prepared, err := productionservice.PreparePolicyVersion("74000000-0000-4000-8000-000000000004", policyValue)
		require.NoError(t, err)
		policy, err := s.PutProductionPolicy(t.Context(), prepared)
		require.NoError(t, err)
		request.PolicyID, request.PolicyVersion = policy.ID, policy.Version
	}
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", request)
	require.NoError(t, err)
	decision := productionAuthorityDecision(member, "74000000-0000-4000-8000-000000000007")
	_, err = s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "74000000-0000-4000-8000-000000000005", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &member}, {Kind: "decision", Decision: &decision}},
	})
	require.NoError(t, err)
	current, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	_, err = s.SealProductionMembership(t.Context(), "test-agent", set.ID, draft.Revision, redaction.MembershipSealRequest{
		OperationID: "74000000-0000-4000-8000-000000000006", ETag: current.ETag,
		Total: 1, MemberHash: current.MemberHash,
	})
	require.NoError(t, err)
	stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
	binding := productionReviewBindingForTest(t, stored, member.ID)
	_, err = s.ReviewProductionMember(t.Context(), "test-agent", set.ID, draft.Revision, ProductionReviewRequest{
		OperationID: "74000000-0000-4000-8000-000000000008", ETag: stored.Draft.ETag,
		MemberID: member.ID, Binding: binding, Complete: true,
	})
	require.NoError(t, err)
	return s, member, set.ID, draft.Revision
}

func TestProductionApprovalSelectionIsExactAndRevocationFailsClosed(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevisionWithPolicy(t, "metadata.title", "standalone", true, false)
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("e4"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	subject, err := productionservice.StoredApprovalSubject(stored)
	require.NoError(t, err)
	require.Equal(t, documentproduction.ApprovalSubjectContractV2, subject.Contract)
	grantRecord, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "74000000-0000-4000-8000-000000000030",
		ApprovalID:  "74000000-0000-4000-8000-000000000031", Subject: subject,
		Evidence: "Synthetic approval evidence.",
	}, stored.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: "2026-09-23T01:00:00Z", EvidenceSHA256: fakeHash("e5"),
		},
	}, time.Date(2026, 9, 23, 1, 1, 0, 0, time.UTC))
	require.NoError(t, err)
	grant, err := s.PutProductionApproval(t.Context(), grantRecord)
	require.NoError(t, err)
	require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{}), ErrInvalidProduction)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{ApprovalID: grant.ID}))
	require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{ApprovalID: "74000000-0000-4000-8000-000000000032"}), ErrInvalidProduction)

	var gateStored productionservice.StoredProductionInputs
	load := func() error {
		return s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			loaded, err := s.LoadProductionGateSnapshot(t.Context(), tx, productionservice.PreparedInputRequest{
				SetID: setID, Revision: revision, PreparedAt: time.Date(2026, 9, 23, 1, 2, 0, 0, time.UTC),
			})
			if err == nil {
				gateStored = loaded
				require.Equal(t, grant.ID, loaded.ApprovalGrant.ID)
				require.Equal(t, documentproduction.ApprovalStateCurrent, loaded.ApprovalEvaluation.State)
			}
			return err
		})
	}
	require.NoError(t, load())
	revisionSHA256, err := productionservice.ProductionRevisionSHA256(gateStored)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	require.NoError(t, err)
	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore,
		productionservice.PreparedInputRequest{
			OperationID: "74000000-0000-4000-8000-000000000035",
			ReceiptID:   "74000000-0000-4000-8000-000000000036",
			SetID:       setID, Revision: revision, ExpectedETag: gateStored.Draft.ETag,
			ExpectedRevisionSHA256: revisionSHA256,
			PreparedAt:             time.Date(2026, 9, 23, 1, 2, 0, 0, time.UTC),
		})
	require.NoError(t, err)
	require.NotNil(t, authority.Receipt)
	require.Equal(t, documentproduction.PreparedProductionContractV2, authority.Prepared.Contract)
	event, err := productionservice.PrepareApprovalEvent(grant, productionservice.ApprovalEventRequest{
		OperationID: "74000000-0000-4000-8000-000000000033",
		EventID:     "74000000-0000-4000-8000-000000000034", Kind: documentproduction.ApprovalEventRevoke,
		EffectiveAt: "2026-09-23T01:01:30Z", Reason: "Synthetic revocation.",
	})
	require.NoError(t, err)
	_, err = s.PutProductionApprovalEvent(t.Context(), event)
	require.NoError(t, err)
	require.ErrorIs(t, load(), ErrInvalidProduction)
}

func TestProductionPrivilegeSelectionChecksFrozenStoredRows(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevisionWithPolicy(t, "metadata.title", "standalone", false, true)
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("e6"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	withheldValue := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "74000000-0000-4000-8000-000000000040", SetID: setID, Revision: revision,
		PolicySHA256: stored.Policy.SHA256,
		Members: []documentproduction.WithheldMember{{
			ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
			SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
			FamilyOrder: 1, Family: member.Family,
		}},
	}
	preparedWithheld, err := productionservice.PrepareWithheldSelection(
		"74000000-0000-4000-8000-000000000041", withheldValue, nil)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), preparedWithheld)
	require.NoError(t, err)
	playersValue := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "74000000-0000-4000-8000-000000000042", Revision: 1,
		Players: []documentproduction.Player{{
			ID: "74000000-0000-4000-8000-000000000043", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: fakeHash("e7"),
		}},
	}
	preparedPlayers, err := productionservice.PreparePlayersSnapshot("74000000-0000-4000-8000-000000000044", playersValue)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), preparedPlayers)
	require.NoError(t, err)
	logID := "74000000-0000-4000-8000-000000000045"
	preparedDraft, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "74000000-0000-4000-8000-000000000046", LogID: logID, Revision: 1,
	}, preparedWithheld.Selection, stored.Policy)
	require.NoError(t, err)
	rows := []documentproduction.PrivilegeRow{{
		ID: "74000000-0000-4000-8000-000000000047", WithheldMemberID: member.ID,
		FamilyOrder: 1, SourceVersionID: member.SourceVersionID,
		Basis: "synthetic_basis", PublicDescription: "Synthetic public description.",
		PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: fakeHash("e8"),
		PersonIDs: []string{playersValue.Players[0].ID},
		Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-23"}},
	}}
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: preparedDraft, PlayersSHA256: preparedPlayers.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: rows,
	})
	require.NoError(t, err)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogValidationRequest{
		OperationID: "74000000-0000-4000-8000-000000000048", LogID: logID, Revision: 1,
		ExpectedGeneration: generation, ValidatedAt: time.Date(2026, 9, 23, 1, 4, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogFreezeRequest{
		OperationID: "74000000-0000-4000-8000-000000000049", LogID: logID, Revision: 1,
		ExpectedGeneration: generation, ExpectedInputsSHA256: validation.Validation.InputsSHA256,
		FrozenAt: time.Date(2026, 9, 23, 1, 5, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{}), ErrInvalidProduction)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{PrivilegeLogID: logID, PrivilegeLogRevision: 1}))
	loaded := productionservice.StoredProductionInputs{}
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var loadErr error
		loaded, loadErr = s.LoadProductionGateSnapshot(t.Context(), tx,
			productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
		return loadErr
	}))
	require.Equal(t, receipt.SHA256, loaded.PrivilegeLog.SHA256)
	revisionSHA256, err := productionservice.ProductionRevisionSHA256(loaded)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	require.NoError(t, err)
	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore,
		productionservice.PreparedInputRequest{
			OperationID: "74000000-0000-4000-8000-000000000054",
			ReceiptID:   "74000000-0000-4000-8000-000000000055",
			SetID:       setID, Revision: revision, ExpectedETag: loaded.Draft.ETag,
			ExpectedRevisionSHA256: revisionSHA256,
			PreparedAt:             time.Date(2026, 9, 23, 1, 6, 0, 0, time.UTC),
		})
	require.NoError(t, err)
	require.NotNil(t, authority.Receipt)
	require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{PrivilegeLogID: "74000000-0000-4000-8000-000000000050", PrivilegeLogRevision: 1}),
		ErrInvalidProduction)
	_, err = s.db.Exec(`DROP TRIGGER production_privilege_log_receipts_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_privilege_log_receipts SET canonical_json=? WHERE log_id=? AND revision=1`,
		[]byte(`{}`), logID)
	require.NoError(t, err)
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, loadErr := s.LoadProductionGateSnapshot(t.Context(), tx,
			productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
		return loadErr
	}), ErrInvalidProduction)
}

func TestProductionCombinedApprovalAndPrivilegeUsesCurrentAdmission(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevisionWithPolicy(t, "metadata.title", "standalone", true, true)
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("ef"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	withheldValue := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "74000000-0000-4000-8000-000000000040", SetID: setID, Revision: revision,
		PolicySHA256: stored.Policy.SHA256,
		Members: []documentproduction.WithheldMember{{
			ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
			SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
			FamilyOrder: 1, Family: member.Family,
		}},
	}
	preparedWithheld, err := productionservice.PrepareWithheldSelection(
		"74000000-0000-4000-8000-000000000041", withheldValue, nil)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), preparedWithheld)
	require.NoError(t, err)
	playersValue := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "74000000-0000-4000-8000-000000000042", Revision: 1,
		Players: []documentproduction.Player{{
			ID: "74000000-0000-4000-8000-000000000043", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: fakeHash("f0"),
		}},
	}
	preparedPlayers, err := productionservice.PreparePlayersSnapshot("74000000-0000-4000-8000-000000000044", playersValue)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), preparedPlayers)
	require.NoError(t, err)
	logID := "74000000-0000-4000-8000-000000000045"
	preparedDraft, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "74000000-0000-4000-8000-000000000046", LogID: logID, Revision: 1,
	}, preparedWithheld.Selection, stored.Policy)
	require.NoError(t, err)
	rows := []documentproduction.PrivilegeRow{{
		ID: "74000000-0000-4000-8000-000000000047", WithheldMemberID: member.ID,
		FamilyOrder: 1, SourceVersionID: member.SourceVersionID,
		Basis: "synthetic_basis", PublicDescription: "Synthetic public description.",
		PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: fakeHash("f1"),
		PersonIDs: []string{playersValue.Players[0].ID},
		Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-23"}},
	}}
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: preparedDraft, PlayersSHA256: preparedPlayers.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: rows,
	})
	require.NoError(t, err)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogValidationRequest{
		OperationID: "74000000-0000-4000-8000-000000000048", LogID: logID, Revision: 1,
		ExpectedGeneration: generation, ValidatedAt: time.Date(2026, 9, 23, 1, 4, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	stored = loadProductionInputsForTest(t, s, setID, revision)
	stored.PrivilegeLog = &documentproduction.PrivilegeLogReceipt{InputsSHA256: validation.Validation.InputsSHA256}
	subject, err := productionservice.StoredApprovalSubject(stored)
	require.NoError(t, err)
	grantRecord, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "74000000-0000-4000-8000-000000000056",
		ApprovalID:  "74000000-0000-4000-8000-000000000057", Subject: subject,
	}, stored.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: "2026-09-23T01:00:00Z", EvidenceSHA256: fakeHash("f2"),
		},
	}, time.Date(2026, 9, 23, 1, 1, 0, 0, time.UTC))
	require.NoError(t, err)
	grant, err := s.PutProductionApproval(t.Context(), grantRecord)
	require.NoError(t, err)
	frozenAt := time.Date(2026, 9, 23, 1, 5, 0, 0, time.UTC)
	freezeEvaluation, err := productionservice.EvaluateRequiredApproval(stored.Policy, subject, &grant, nil, frozenAt)
	require.NoError(t, err)
	require.NoError(t, s.BindPrivilegeLogApproval(t.Context(), PrivilegeLogApprovalBinding{
		OperationID: "74000000-0000-4000-8000-000000000058", LogID: logID, Revision: 1,
		ExpectedGeneration: generation, ApprovalID: grant.ID, Evaluation: freezeEvaluation,
	}))
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogFreezeRequest{
		OperationID: "74000000-0000-4000-8000-000000000049", LogID: logID, Revision: 1,
		ExpectedGeneration: generation, ExpectedInputsSHA256: validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: freezeEvaluation.SHA256, FrozenAt: frozenAt,
	})
	require.NoError(t, err)
	require.Equal(t, freezeEvaluation.SHA256, receipt.ApprovalEvaluationSHA256)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{ApprovalID: grant.ID, PrivilegeLogID: logID, PrivilegeLogRevision: 1}))
	preparedAt := time.Date(2026, 9, 23, 1, 6, 0, 0, time.UTC)
	var loaded productionservice.StoredProductionInputs
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var loadErr error
		loaded, loadErr = s.LoadProductionGateSnapshot(t.Context(), tx,
			productionservice.PreparedInputRequest{SetID: setID, Revision: revision, PreparedAt: preparedAt})
		return loadErr
	}))
	require.NotEqual(t, freezeEvaluation.SHA256, loaded.ApprovalEvaluation.SHA256)
	revisionSHA256, err := productionservice.ProductionRevisionSHA256(loaded)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	require.NoError(t, err)
	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore,
		productionservice.PreparedInputRequest{
			OperationID: "74000000-0000-4000-8000-000000000059",
			ReceiptID:   "74000000-0000-4000-8000-000000000060", SetID: setID, Revision: revision,
			ExpectedETag: loaded.Draft.ETag, ExpectedRevisionSHA256: revisionSHA256, PreparedAt: preparedAt,
		})
	require.NoError(t, err)
	require.NotNil(t, authority.Receipt)
	require.Equal(t, loaded.ApprovalEvaluation.SHA256, authority.Receipt.ApprovalEvaluationSHA256)
}

func TestProductionEmailPublicationRequiresExactOperation(t *testing.T) {
	s, member, setID, revision := productionEmailEvidenceRevision(t)
	view, err := s.EmailMetadata(t.Context(), member.SourceVersionID)
	require.NoError(t, err)
	firstOperation := "74000000-0000-4000-8000-000000000020"
	secondOperation := "74000000-0000-4000-8000-000000000021"
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, firstOperation))
	require.NoError(t, err)
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, secondOperation))
	require.NoError(t, err)

	require.ErrorIs(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil), ErrInvalidProduction)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_revision_email_publications WHERE set_id=? AND revision=?`,
		setID, revision).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision,
		map[string]string{member.Family.RootVersionID: firstOperation}))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	require.Equal(t, firstOperation, stored.Members[0].EvidencePin.EmailPublicationOperationID)
	require.True(t, stored.Members[0].Facts.FamilyComplete)
	require.ErrorIs(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision,
		map[string]string{member.Family.RootVersionID: secondOperation}), ErrInvalidProduction)
	_, err = s.db.Exec(`UPDATE email_document_publications SET receipt_json=replace(receipt_json,'complete','partial') WHERE operation_id=?`, firstOperation)
	require.NoError(t, err)
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := s.LoadProductionGateSnapshot(t.Context(), tx, productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
		return err
	}), ErrInvalidProduction)
}

func TestProductionEmailGateRejectsMissingPublicationPin(t *testing.T) {
	s, _, setID, revision := productionEmailEvidenceRevision(t)
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := s.LoadProductionGateSnapshot(t.Context(), tx,
			productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
		return err
	}), ErrInvalidProduction)
}

func TestProductionGateSelectionRequiresEvidenceBeforeWritingSelector(t *testing.T) {
	t.Run("email publication", func(t *testing.T) {
		s, _, setID, revision := productionEmailEvidenceRevision(t)
		require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
			ProductionRevisionGateSelection{}), ErrInvalidProduction)
		var count int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_revision_gate_authority
			WHERE set_id=? AND revision=?`, setID, revision).Scan(&count))
		require.Zero(t, count)
	})
	t.Run("metadata facts", func(t *testing.T) {
		s, member, setID, revision := productionEvidenceRevision(t, "metadata.title")
		productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("f3"), "Synthetic title")
		require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
			ProductionRevisionGateSelection{}), ErrInvalidProduction)
		var count int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_revision_gate_authority
			WHERE set_id=? AND revision=?`, setID, revision).Scan(&count))
		require.Zero(t, count)
	})
	t.Run("generic standalone", func(t *testing.T) {
		s, _, setID, revision := productionEvidenceRevision(t, "")
		require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
			ProductionRevisionGateSelection{}))
		require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil),
			"an empty replay does not change generic gate evidence")
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			_, err := s.LoadProductionGateSnapshot(t.Context(), tx,
				productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
			return err
		}))
	})
}

func TestProductionPinCannotChangeEvidenceAfterGateSelection(t *testing.T) {
	s, member, setID, revision := productionEmailEvidenceRevision(t)
	view, err := s.EmailMetadata(t.Context(), member.SourceVersionID)
	require.NoError(t, err)
	operationID := "74000000-0000-4000-8000-000000000062"
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, operationID))
	require.NoError(t, err)
	choice := map[string]string{member.Family.RootVersionID: operationID}
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, choice))
	before := loadProductionInputsForTest(t, s, setID, revision)
	beforeSubject, err := productionservice.StoredApprovalSubject(before)
	require.NoError(t, err)
	_, beforeDigest, err := documentproduction.CanonicalApprovalSubject(beforeSubject)
	require.NoError(t, err)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{}))
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, choice),
		"an exact replay must leave pinned gate evidence unchanged")
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("f4"), "Synthetic title")
	require.ErrorIs(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, choice), ErrInvalidProduction)
	var factsCount int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_member_policy_facts
		WHERE set_id=? AND revision=?`, setID, revision).Scan(&factsCount))
	require.Zero(t, factsCount)
	after := loadProductionInputsForTest(t, s, setID, revision)
	afterSubject, err := productionservice.StoredApprovalSubject(after)
	require.NoError(t, err)
	_, afterDigest, err := documentproduction.CanonicalApprovalSubject(afterSubject)
	require.NoError(t, err)
	require.Equal(t, beforeDigest, afterDigest)
}

func TestProductionEmailPinAcceptsExistingOperationIDGrammar(t *testing.T) {
	s, member, setID, revision := productionEmailEvidenceRevision(t)
	view, err := s.EmailMetadata(t.Context(), member.SourceVersionID)
	require.NoError(t, err)
	operationID := "synthetic.publication-1"
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, operationID))
	require.NoError(t, err)
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision,
		map[string]string{member.Family.RootVersionID: operationID}))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	require.Equal(t, operationID, stored.Members[0].EvidencePin.EmailPublicationOperationID)
}

func TestProductionEmailPinRejectsMissingRelationRow(t *testing.T) {
	s, member, setID, revision := productionEmailEvidenceRevision(t)
	view, err := s.EmailMetadata(t.Context(), member.SourceVersionID)
	require.NoError(t, err)
	operationID := "74000000-0000-4000-8000-000000000061"
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, operationID))
	require.NoError(t, err)
	require.NotEmpty(t, receipt.Relations)
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision,
		map[string]string{member.Family.RootVersionID: operationID}))
	_, err = s.db.Exec(`DELETE FROM email_document_relations WHERE operation_id=? AND occurrence_order=?`,
		operationID, receipt.Relations[0].Order)
	require.NoError(t, err)
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, loadErr := s.LoadProductionGateSnapshot(t.Context(), tx,
			productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
		return loadErr
	}), ErrInvalidProduction)
}

func productionEvidenceMetadata(t *testing.T, s *Store, sourceSHA256, fingerprint, title string) SourceMetadataGeneration {
	t.Helper()
	record := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{
			{Key: "title", Namespace: "pdf.info", SourceField: "Title",
				Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(title)}},
			{Key: "email.bcc", Namespace: "email", SourceField: "Bcc", Sensitive: true,
				Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new("private@example.test")}},
			{Key: "office.custom.secret", Namespace: "office.custom", SourceField: "Secret",
				Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new("synthetic secret")}},
		}}
	canonical, _, err := document.MarshalSourceMetadataV1(record)
	require.NoError(t, err)
	generation, err := s.PublishSourceMetadata(t.Context(), sourceSHA256, fingerprint, canonical)
	require.NoError(t, err)
	return generation
}

func TestProductionPinnedPolicyFactsAreAllowlistedAndFresh(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevision(t, "metadata.title")
	first := productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("e1"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))

	stored := loadProductionInputsForTest(t, s, setID, revision)
	require.Equal(t, []string{"Synthetic title"}, stored.Members[0].Facts.Fields["metadata.title"])
	require.NotContains(t, stored.Members[0].Facts.Fields, "email.bcc")
	require.NotContains(t, stored.Members[0].Facts.Fields, "office.custom.secret")
	require.Equal(t, first.GenerationID, stored.Members[0].EvidencePin.SourceMetadataGenerationID)
	require.Equal(t, first.Checksum, stored.Members[0].EvidencePin.SourceMetadataEvidenceSHA256)
	require.Equal(t, documentproduction.PolicyFactsAllowlistV1, stored.Members[0].EvidencePin.AllowlistVersion)

	_, err := productionservice.ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	reopened, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	reloaded := loadProductionInputsForTest(t, reopened, setID, revision)
	require.Equal(t, stored.Members[0].Facts, reloaded.Members[0].Facts)
	require.Equal(t, *stored.Members[0].EvidencePin, *reloaded.Members[0].EvidencePin)
	second := productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("e2"), "Different title")
	require.NotEqual(t, first.GenerationID, second.GenerationID)
	historical := loadProductionInputsForTest(t, s, setID, revision)
	require.Equal(t, stored.Members[0].Facts, historical.Members[0].Facts)
	require.Equal(t, *stored.Members[0].EvidencePin, *historical.Members[0].EvidencePin)
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := s.LoadProductionGateSnapshot(t.Context(), tx, productionservice.PreparedInputRequest{
			SetID: setID, Revision: revision, PreparedAt: time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
		})
		return err
	}), ErrInvalidProduction)
}

func TestProductionGateSelectionRejectsStaleSourceMetadataHead(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevision(t, "metadata.title")
	first := productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("f5"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	second := productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("f6"), "Updated synthetic title")
	require.NotEqual(t, first.GenerationID, second.GenerationID)

	require.ErrorIs(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{}), ErrInvalidProduction)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_revision_gate_authority
		WHERE set_id=? AND revision=?`, setID, revision).Scan(&count))
	require.Zero(t, count)
}

func TestProductionConfiguredPolicyRejectsUnsupportedField(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevision(t, "document.scope")
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("e3"), "Synthetic title")
	require.ErrorIs(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil), ErrInvalidProduction)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_member_policy_facts WHERE set_id=? AND revision=?`,
		setID, revision).Scan(&count))
	require.Zero(t, count)
}

func TestProductionConfiguredPolicyRequiresAndVerifiesSourceGeneration(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevision(t, "metadata.title")
	require.ErrorIs(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil), ErrInvalidProduction)
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("e9"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	_, err := s.db.Exec(`DROP TRIGGER source_metadata_generations_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE source_metadata_generations SET checksum=? WHERE source_sha256=?`,
		fakeHash("ea"), member.SourceSHA256)
	require.NoError(t, err)
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := s.LoadProductionGateSnapshot(t.Context(), tx,
			productionservice.PreparedInputRequest{SetID: setID, Revision: revision})
		return err
	}), ErrInvalidProduction)
}

func TestProductionSensitiveAllowlistedSourceFieldStaysAbsent(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevision(t, "metadata.title")
	metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{
			Key: "title", Namespace: "pdf.info", SourceField: "Title", Sensitive: true,
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new("synthetic confidential title")},
		}}}
	raw, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), member.SourceSHA256, fakeHash("ee"), raw)
	require.NoError(t, err)
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	require.NotContains(t, stored.Members[0].Facts.Fields, "metadata.title")
}

func TestProductionGateMetadataRejectsHalfPopulatedSelectors(t *testing.T) {
	for _, column := range []string{"approval_id", "privilege_log_id"} {
		t.Run(column, func(t *testing.T) {
			s, _, setID, revision := productionEvidenceRevision(t, "")
			_, err := s.db.Exec(`INSERT INTO production_revision_gate_authority
				(set_id,revision,`+column+`) VALUES(?,?,?)`, setID, revision,
				"74000000-0000-4000-8000-000000000051")
			require.NoError(t, err)
			require.ErrorIs(t, validateProductionGateAuthorityState(t.Context(), s.db), ErrInvalidProduction)
			require.ErrorIs(t, validateProductionMetadataState(t.Context(), s.db), ErrInvalidProduction)
		})
	}
}

func TestProductionGateMetadataRejectsWrongGrantSubject(t *testing.T) {
	s, member, setID, revision := productionEvidenceRevisionWithPolicy(t, "metadata.title", "standalone", true, false)
	productionEvidenceMetadata(t, s, member.SourceSHA256, fakeHash("eb"), "Synthetic title")
	require.NoError(t, s.PinProductionRevisionEvidence(t.Context(), setID, revision, nil))
	stored := loadProductionInputsForTest(t, s, setID, revision)
	subject, err := productionservice.StoredApprovalSubject(stored)
	require.NoError(t, err)
	grantRecord, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "74000000-0000-4000-8000-000000000052",
		ApprovalID:  "74000000-0000-4000-8000-000000000053", Subject: subject,
	}, stored.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: "2026-09-23T01:00:00Z", EvidenceSHA256: fakeHash("ec"),
		},
	}, time.Date(2026, 9, 23, 1, 1, 0, 0, time.UTC))
	require.NoError(t, err)
	grant, err := s.PutProductionApproval(t.Context(), grantRecord)
	require.NoError(t, err)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), setID, revision,
		ProductionRevisionGateSelection{ApprovalID: grant.ID}))
	_, err = s.db.Exec(`DROP TRIGGER production_revision_gate_authority_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_revision_gate_authority SET approval_subject_sha256=? WHERE set_id=? AND revision=?`,
		fakeHash("ed"), setID, revision)
	require.NoError(t, err)
	require.ErrorIs(t, validateProductionGateAuthorityState(t.Context(), s.db), ErrInvalidProduction)
}
