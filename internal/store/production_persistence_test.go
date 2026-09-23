package store

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestProductionAuthorityFreezesStoredRowsAndReplaysOperations(t *testing.T) {
	s, fixture := newProductionPersistenceFixture(t)

	policy, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	require.Equal(t, fixture.policy.PolicySHA256, policy.SHA256)
	retry, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	require.Equal(t, policy, retry)
	changedPolicy := fixture.policy
	changedPolicy.RequestSHA256 = productionPersistenceSHA("f")
	_, err = s.PutProductionPolicy(t.Context(), changedPolicy)
	requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)

	_, err = s.PutProductionPlayersSnapshot(t.Context(), fixture.players)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), fixture.withheld)
	require.NoError(t, err)
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: fixture.draft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), generation)
	duplicateDraft := fixture.draft
	duplicateDraft.OperationID = "24242424-2424-4424-8424-242424242424"
	_, err = s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: duplicateDraft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)
	var duplicateOperationReceipts int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		duplicateDraft.OperationID).Scan(&duplicateOperationReceipts))
	require.Zero(t, duplicateOperationReceipts, "a conflicting operation must not receive a replay receipt")

	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogValidationRequest{
		OperationID: "77777777-7777-4777-8777-777777777777", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ValidatedAt: productionPersistenceTime(t, "2026-09-22T14:00:00Z"),
	})
	require.NoError(t, err)

	approval := fixture.approval(t, validation.Validation.InputsSHA256)
	grant, err := s.PutProductionApproval(t.Context(), approval.record)
	require.NoError(t, err)
	evaluation, err := productionservice.EvaluateRequiredApproval(policy, approval.record.Subject,
		&grant, nil, productionPersistenceTime(t, "2026-09-22T14:00:02Z"))
	require.NoError(t, err)
	forgedEvaluation := evaluation
	forgedEvaluation.State = documentproduction.ApprovalStateRevoked
	forgedEvaluation.SHA256 = ""
	_, forgedEvaluation.SHA256, err = documentproduction.CanonicalApprovalEvaluation(forgedEvaluation)
	require.NoError(t, err)
	err = s.BindPrivilegeLogApproval(t.Context(), PrivilegeLogApprovalBinding{
		OperationID: "25252525-2525-4525-8525-252525252525",
		LogID:       fixture.draft.LogID, Revision: fixture.draft.Revision,
		ExpectedGeneration: generation, ApprovalID: grant.ID, Evaluation: forgedEvaluation,
	})
	require.Error(t, err, "storage must re-evaluate approval authority instead of trusting a canonical caller value")
	require.NoError(t, s.BindPrivilegeLogApproval(t.Context(), PrivilegeLogApprovalBinding{
		OperationID: "17171717-1717-4717-8717-171717171717",
		LogID:       fixture.draft.LogID, Revision: fixture.draft.Revision,
		ExpectedGeneration: generation, ApprovalID: grant.ID, Evaluation: evaluation,
	}))

	freeze := productionservice.PrivilegeLogFreezeRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ExpectedInputsSHA256:             validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: evaluation.SHA256,
		FrozenAt:                         productionPersistenceTime(t, "2026-09-22T14:00:02Z"),
	}
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, freeze)
	require.NoError(t, err)
	replayed, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, freeze)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = s.db.Exec(`INSERT INTO production_privilege_log_rows
		(log_id,revision,row_ordinal,row_id,canonical_json) VALUES(?,?,?,?,?)`,
		fixture.draft.LogID, fixture.draft.Revision, 2,
		"21212121-2121-4121-8121-212121212121", []byte(`{}`))
	require.Error(t, err, "the schema must reject direct row insertion after freeze")
	_, err = s.db.Exec(`UPDATE production_privilege_log_approvals SET evaluation_sha256=?
		WHERE log_id=? AND revision=?`, productionPersistenceSHA("f"), fixture.draft.LogID, fixture.draft.Revision)
	require.Error(t, err, "the schema must reject direct approval changes after freeze")

	changedFreeze := freeze
	changedFreeze.FrozenAt = changedFreeze.FrozenAt.Add(time.Second)
	_, err = productionservice.FreezeStoredPrivilegeLog(t.Context(), s, changedFreeze)
	requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)
	stored, err := s.PrivilegeLogReceipt(t.Context(), fixture.draft.LogID, fixture.draft.Revision)
	require.NoError(t, err)
	require.Equal(t, receipt, stored)
	attachment, err := productionservice.PreparePrivilegeLogAttachment(
		"19191919-1919-4919-8919-191919191919", "20202020-2020-4020-8020-202020202020",
		receipt, fixture.withheld.Selection, fixture.rows, productionPersistenceSHA("1"),
		[]documentproduction.PrivilegeOutputReference{{
			WithheldMemberID: fixture.withheld.Selection.Members[0].ID,
			AssignedNumber:   "SYNTHETIC0001", ArtifactSHA256: productionPersistenceSHA("2"),
		}}, productionPersistenceTime(t, "2026-09-22T14:00:03Z"))
	require.NoError(t, err)
	storedAttachment, err := s.PutPrivilegeLogAttachment(t.Context(), attachment)
	require.NoError(t, err)
	require.Equal(t, attachment.Receipt, storedAttachment)
	replayedAttachment, err := s.PutPrivilegeLogAttachment(t.Context(), attachment)
	require.NoError(t, err)
	require.Equal(t, storedAttachment, replayedAttachment)

	_, err = s.ReplacePrivilegeLogRows(t.Context(), PrivilegeLogRowUpdate{
		OperationID: "18181818-1818-4818-8818-181818181818",
		LogID:       fixture.draft.LogID, Revision: fixture.draft.Revision,
		ExpectedGeneration: generation, Rows: fixture.rows,
	})
	require.Error(t, err, "frozen rows are immutable")
	publicRows, err := s.PrivilegeLogPublicRows(t.Context(), fixture.draft.LogID, fixture.draft.Revision)
	require.NoError(t, err)
	publicJSON, err := json.Marshal(publicRows)
	require.NoError(t, err)
	require.NotContains(t, string(publicJSON), "private_rationale")
	require.NotContains(t, string(publicJSON), "evidence_sha256")
	require.NotContains(t, string(publicJSON), "Synthetic private rationale")

	publicGrant, publicEvents, err := s.ProductionApprovalPublic(t.Context(), grant.ID)
	require.NoError(t, err)
	approvalJSON, err := json.Marshal(struct {
		Grant  documentproduction.ApprovalPublicGrant   `json:"grant"`
		Events []documentproduction.ApprovalPublicEvent `json:"events"`
	}{publicGrant, publicEvents})
	require.NoError(t, err)
	require.NotContains(t, string(approvalJSON), "evidence")
	require.NotContains(t, string(approvalJSON), "reason")

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restoredStore := newTestStore(t)
	require.NoError(t, restoredStore.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	restoredReceipt, err := restoredStore.PrivilegeLogReceipt(t.Context(), fixture.draft.LogID, fixture.draft.Revision)
	require.NoError(t, err)
	require.Equal(t, receipt, restoredReceipt)
	var attachmentCount int
	require.NoError(t, restoredStore.db.QueryRow(`SELECT COUNT(*) FROM production_privilege_log_attachments
		WHERE sha256=?`, attachment.Receipt.SHA256).Scan(&attachmentCount))
	require.Equal(t, 1, attachmentCount)
}

func TestProductionFreezeReevaluatesStoredApprovalEventsWithoutPartialReceipt(t *testing.T) {
	s, fixture := newProductionPersistenceFixture(t)
	policy, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), fixture.players)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), fixture.withheld)
	require.NoError(t, err)
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: fixture.draft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	require.NoError(t, err)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogValidationRequest{
		OperationID: "77777777-7777-4777-8777-777777777777", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ValidatedAt: productionPersistenceTime(t, "2026-09-22T14:00:00Z"),
	})
	require.NoError(t, err)
	approval := fixture.approval(t, validation.Validation.InputsSHA256)
	grant, err := s.PutProductionApproval(t.Context(), approval.record)
	require.NoError(t, err)
	evaluation, err := productionservice.EvaluateRequiredApproval(policy, approval.record.Subject,
		&grant, nil, productionPersistenceTime(t, "2026-09-22T14:00:01Z"))
	require.NoError(t, err)
	require.NoError(t, s.BindPrivilegeLogApproval(t.Context(), PrivilegeLogApprovalBinding{
		OperationID: "17171717-1717-4717-8717-171717171717",
		LogID:       fixture.draft.LogID, Revision: fixture.draft.Revision,
		ExpectedGeneration: generation, ApprovalID: grant.ID, Evaluation: evaluation,
	}))
	event, err := productionservice.PrepareApprovalEvent(grant, productionservice.ApprovalEventRequest{
		OperationID: "99999999-9999-4999-8999-999999999999",
		EventID:     "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Kind: documentproduction.ApprovalEventRevoke,
		EffectiveAt: "2026-09-22T14:00:01.5Z", Reason: "Synthetic private reason.",
	})
	require.NoError(t, err)
	_, err = s.PutProductionApprovalEvent(t.Context(), event)
	require.NoError(t, err)

	_, err = productionservice.FreezeStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogFreezeRequest{
		OperationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ExpectedInputsSHA256:             validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: evaluation.SHA256,
		FrozenAt:                         productionPersistenceTime(t, "2026-09-22T14:00:02Z"),
	})
	requireProductionProblem(t, err, documentproduction.ProblemApprovalStale)
	_, err = s.PrivilegeLogReceipt(t.Context(), fixture.draft.LogID, fixture.draft.Revision)
	require.ErrorIs(t, err, ErrNotFound)
	var receipts, operations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_privilege_log_receipts`).Scan(&receipts))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb").Scan(&operations))
	require.Zero(t, receipts)
	require.Zero(t, operations)
}

func TestProductionRowGenerationGuardsRetainReplayReceipts(t *testing.T) {
	s, fixture := newProductionPersistenceFixture(t)
	_, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), fixture.players)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), fixture.withheld)
	require.NoError(t, err)
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: fixture.draft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	require.NoError(t, err)
	firstRequest := productionservice.PrivilegeLogValidationRequest{
		OperationID: "77777777-7777-4777-8777-777777777777", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ValidatedAt: productionPersistenceTime(t, "2026-09-22T14:00:00Z"),
	}
	first, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, firstRequest)
	require.NoError(t, err)

	changedRows := append([]documentproduction.PrivilegeRow(nil), fixture.rows...)
	changedRows[0].PublicDescription = "Changed synthetic public description."
	update := PrivilegeLogRowUpdate{
		OperationID: "18181818-1818-4818-8818-181818181818",
		LogID:       fixture.draft.LogID, Revision: fixture.draft.Revision,
		ExpectedGeneration: generation, Rows: changedRows,
	}
	nextGeneration, err := s.ReplacePrivilegeLogRows(t.Context(), update)
	require.NoError(t, err)
	require.Equal(t, int64(2), nextGeneration)
	replayedGeneration, err := s.ReplacePrivilegeLogRows(t.Context(), update)
	require.NoError(t, err)
	require.Equal(t, nextGeneration, replayedGeneration)

	second, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogValidationRequest{
		OperationID: "23232323-2323-4323-8323-232323232323", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: nextGeneration,
		ValidatedAt: productionPersistenceTime(t, "2026-09-22T14:00:01Z"),
	})
	require.NoError(t, err)
	require.NotEqual(t, first.Validation.RowsSHA256, second.Validation.RowsSHA256)
	replayedFirst, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, firstRequest)
	require.NoError(t, err)
	require.Equal(t, first, replayedFirst, "an exact operation retry retains its original receipt")

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported),
		"historical operation receipts remain valid after current validation advances")
}

func TestProductionInsertFailuresRemainStorageErrors(t *testing.T) {
	s, fixture := newProductionPersistenceFixture(t)
	policy, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), fixture.players)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), fixture.withheld)
	require.NoError(t, err)

	installProductionInsertFailure(t, s, "synthetic_fail_draft_insert",
		"production_privilege_log_drafts", "synthetic draft insert failure")
	_, err = s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: fixture.draft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	requireProductionStorageError(t, err, "synthetic draft insert failure")
	dropProductionTrigger(t, s, "synthetic_fail_draft_insert")

	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: fixture.draft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	require.NoError(t, err)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogValidationRequest{
		OperationID: "77777777-7777-4777-8777-777777777777", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ValidatedAt: productionPersistenceTime(t, "2026-09-22T14:00:00Z"),
	})
	require.NoError(t, err)
	approval := fixture.approval(t, validation.Validation.InputsSHA256)
	grant, err := s.PutProductionApproval(t.Context(), approval.record)
	require.NoError(t, err)
	evaluation, err := productionservice.EvaluateRequiredApproval(policy, approval.record.Subject,
		&grant, nil, productionPersistenceTime(t, "2026-09-22T14:00:02Z"))
	require.NoError(t, err)
	require.NoError(t, s.BindPrivilegeLogApproval(t.Context(), PrivilegeLogApprovalBinding{
		OperationID: "17171717-1717-4717-8717-171717171717",
		LogID:       fixture.draft.LogID, Revision: fixture.draft.Revision,
		ExpectedGeneration: generation, ApprovalID: grant.ID, Evaluation: evaluation,
	}))
	freeze := productionservice.PrivilegeLogFreezeRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", LogID: fixture.draft.LogID,
		Revision: fixture.draft.Revision, ExpectedGeneration: generation,
		ExpectedInputsSHA256:             validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: evaluation.SHA256,
		FrozenAt:                         productionPersistenceTime(t, "2026-09-22T14:00:02Z"),
	}
	installProductionInsertFailure(t, s, "synthetic_fail_receipt_insert",
		"production_privilege_log_receipts", "synthetic receipt insert failure")
	_, err = productionservice.FreezeStoredPrivilegeLog(t.Context(), s, freeze)
	requireProductionStorageError(t, err, "synthetic receipt insert failure")
	dropProductionTrigger(t, s, "synthetic_fail_receipt_insert")

	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, freeze)
	require.NoError(t, err)
	attachment, err := productionservice.PreparePrivilegeLogAttachment(
		"19191919-1919-4919-8919-191919191919", "20202020-2020-4020-8020-202020202020",
		receipt, fixture.withheld.Selection, fixture.rows, productionPersistenceSHA("1"),
		[]documentproduction.PrivilegeOutputReference{{
			WithheldMemberID: fixture.withheld.Selection.Members[0].ID,
			AssignedNumber:   "SYNTHETIC0001", ArtifactSHA256: productionPersistenceSHA("2"),
		}}, productionPersistenceTime(t, "2026-09-22T14:00:03Z"))
	require.NoError(t, err)
	installProductionInsertFailure(t, s, "synthetic_fail_attachment_insert",
		"production_privilege_log_attachments", "synthetic attachment insert failure")
	_, err = s.PutPrivilegeLogAttachment(t.Context(), attachment)
	requireProductionStorageError(t, err, "synthetic attachment insert failure")
	dropProductionTrigger(t, s, "synthetic_fail_attachment_insert")
	storedAttachment, err := s.PutPrivilegeLogAttachment(t.Context(), attachment)
	require.NoError(t, err)
	require.Equal(t, attachment.Receipt, storedAttachment)
}

func TestProductionMetadataRoundTripRevalidatesCanonicalAuthority(t *testing.T) {
	s, fixture := newProductionPersistenceFixture(t)
	_, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), fixture.players)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), fixture.withheld)
	require.NoError(t, err)
	_, err = s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: fixture.draft, PlayersSHA256: fixture.players.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: fixture.rows,
	})
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &backup))
	require.NoError(t, snapshot.Close())
	require.Equal(t, exported.Bytes(), backup.Bytes(), "production authority is backup authority")

	for _, data := range [][]byte{exported.Bytes(), backup.Bytes()} {
		target := newTestStore(t)
		require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(data)))
		var restored bytes.Buffer
		require.NoError(t, target.ExportMetadata(t.Context(), &restored))
		require.Equal(t, exported.Bytes(), restored.Bytes())
	}

	tampered := strings.Replace(exported.String(), fixture.policy.PolicySHA256,
		productionPersistenceSHA("e"), 1)
	target := newTestStore(t)
	err = target.ImportMetadata(t.Context(), strings.NewReader(tampered))
	require.Error(t, err, "restore must recompute canonical digests and relationships")
	var count int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM production_policy_versions`).Scan(&count))
	require.Zero(t, count, "invalid restore rolls back atomically")

	_, err = io.Copy(io.Discard, bytes.NewReader(exported.Bytes()))
	require.NoError(t, err)
}

type productionPersistenceFixture struct {
	policy   productionservice.PreparedPolicyVersion
	players  productionservice.PreparedPlayersSnapshot
	withheld productionservice.PreparedWithheldSelection
	draft    productionservice.PreparedPrivilegeLogDraft
	rows     []documentproduction.PrivilegeRow
}

type productionPersistenceApproval struct {
	record productionservice.ApprovalRecord
}

func newProductionPersistenceFixture(t *testing.T) (*Store, productionPersistenceFixture) {
	t.Helper()
	s := newTestStore(t)
	policyValue := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1, ID: "11111111-1111-4111-8111-111111111111", Version: 1,
		Name: "Synthetic policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}},
		Approval: documentproduction.ApprovalRequirement{Required: true, EvidenceRequired: true, MaxAgeSeconds: 3600},
		PrivilegeLog: documentproduction.PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
			RequiredFields: []string{"date"}, AllowedBases: []string{"synthetic_basis"}},
		ConflictMode: documentproduction.PolicyConflictReject,
	}
	policy, err := productionservice.PreparePolicyVersion("22222222-2222-4222-8222-222222222222", policyValue)
	require.NoError(t, err)
	playersValue := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "33333333-3333-4333-8333-333333333333", Revision: 1,
		Players: []documentproduction.Player{{
			ID: "44444444-4444-4444-8444-444444444444", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: productionPersistenceSHA("1"),
		}},
	}
	players, err := productionservice.PreparePlayersSnapshot("55555555-5555-4555-8555-555555555555", playersValue)
	require.NoError(t, err)
	withheldValue := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "66666666-6666-4666-8666-666666666666", SetID: "abababab-abab-4bab-8bab-abababababab",
		Revision: 1, PolicySHA256: policy.PolicySHA256,
		Members: []documentproduction.WithheldMember{{
			ID: "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd", Ordinal: 1,
			SourceVersionID: "dededede-dede-4ede-8ede-dededededede", SourceSHA256: productionPersistenceSHA("2"), SourceSize: 10,
			FamilyOrder: 1, Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "dededede-dede-4ede-8ede-dededededede"},
		}},
	}
	withheld, err := productionservice.PrepareWithheldSelection("efefefef-efef-4fef-8fef-efefefefefef", withheldValue, nil)
	require.NoError(t, err)
	draft, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "12121212-1212-4212-8212-121212121212",
		LogID:       "13131313-1313-4313-8313-131313131313", Revision: 1,
	}, withheld.Selection, policy.Policy)
	require.NoError(t, err)
	rows := []documentproduction.PrivilegeRow{{
		ID: "14141414-1414-4414-8414-141414141414", WithheldMemberID: withheld.Selection.Members[0].ID,
		FamilyOrder: 1, SourceVersionID: withheld.Selection.Members[0].SourceVersionID,
		Basis: "synthetic_basis", PublicDescription: "Synthetic public description.",
		PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: productionPersistenceSHA("3"),
		PersonIDs: []string{players.Snapshot.Players[0].ID},
		Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}},
	}}
	return s, productionPersistenceFixture{policy: policy, players: players, withheld: withheld, draft: draft, rows: rows}
}

func (f productionPersistenceFixture) approval(t *testing.T, inputsSHA256 string) productionPersistenceApproval {
	t.Helper()
	subject := documentproduction.ApprovalSubject{
		Contract: documentproduction.ApprovalSubjectContractV1,
		SetID:    f.withheld.Selection.SetID, Revision: f.withheld.Selection.Revision,
		Members: []documentproduction.ApprovalMember{{
			MemberID: f.withheld.Selection.Members[0].ID, Ordinal: 1,
			SourceVersionID: f.withheld.Selection.Members[0].SourceVersionID,
			SourceSHA256:    f.withheld.Selection.Members[0].SourceSHA256, SourceSize: f.withheld.Selection.Members[0].SourceSize,
			PDFSHA256: productionPersistenceSHA("4"), PageInventorySHA256: productionPersistenceSHA("5"),
			MapSHA256: productionPersistenceSHA("6"), DecisionsSHA256: productionPersistenceSHA("7"),
			ResolvedSHA256: productionPersistenceSHA("8"),
		}},
		InstructionsSHA256: productionPersistenceSHA("9"), RecipeSHA256: productionPersistenceSHA("a"),
		OutputProfileSHA256: productionPersistenceSHA("b"), DisclosureProfileSHA256: productionPersistenceSHA("c"),
		NumberingPolicySHA256:   productionPersistenceSHA("d"),
		Policy:                  redaction.PolicySelection{PolicyID: f.policy.Policy.ID, Version: f.policy.Policy.Version, PolicySHA256: f.policy.PolicySHA256},
		WithheldSelectionSHA256: f.withheld.SelectionSHA256, PrivilegeLogInputsSHA256: inputsSHA256,
	}
	record, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "15151515-1515-4515-8515-151515151515",
		ApprovalID:  "16161616-1616-4616-8616-161616161616", Subject: subject,
		Evidence: "Synthetic private evidence.",
	}, f.policy.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: "2026-09-22T13:59:00Z", EvidenceSHA256: productionPersistenceSHA("e"),
		},
	}, productionPersistenceTime(t, "2026-09-22T14:00:00Z"))
	require.NoError(t, err)
	return productionPersistenceApproval{record: record}
}

func productionPersistenceSHA(character string) string { return strings.Repeat(character, 64) }

func productionPersistenceTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

func installProductionInsertFailure(t *testing.T, s *Store, name, table, message string) {
	t.Helper()
	_, err := s.db.Exec(`CREATE TRIGGER ` + name + ` BEFORE INSERT ON ` + table +
		` BEGIN SELECT RAISE(ABORT, '` + message + `'); END`)
	require.NoError(t, err)
}

func dropProductionTrigger(t *testing.T, s *Store, name string) {
	t.Helper()
	_, err := s.db.Exec(`DROP TRIGGER ` + name)
	require.NoError(t, err)
}

func requireProductionStorageError(t *testing.T, err error, message string) {
	t.Helper()
	require.ErrorContains(t, err, message)
	problem := &documentproduction.Problem{}
	require.NotErrorAs(t, err, &problem, "unexpected semantic problem: %v", problem)
}

func requireProductionProblem(t *testing.T, err error, code documentproduction.ProblemCode) {
	t.Helper()
	require.Error(t, err)
	problem := &documentproduction.Problem{}
	ok := errors.As(err, &problem)
	require.True(t, ok, "expected production problem, got %T: %v", err, err)
	require.Equal(t, code, problem.Code)
}
