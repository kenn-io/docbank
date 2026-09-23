package production

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
)

func TestPrivilegeValidationReadsStoredRows(t *testing.T) {
	store := newPrivilegeTestStore(t)
	callerValidRows := clonePrivilegeRows(store.draft.Rows)
	store.draft.Rows[0].Basis = "invented_basis"

	_, err := ValidateStoredPrivilegeLog(t.Context(), store, PrivilegeLogValidationRequest{
		OperationID: "11111111-1111-4111-8111-111111111111", LogID: store.draft.LogID,
		Revision: store.draft.Revision, ExpectedGeneration: store.draft.Generation,
		ValidatedAt: mustApprovalTime(t, "2026-09-22T03:00:00Z"),
	})
	require.Error(t, err)
	require.Nil(t, store.validation, "invalid stored rows cannot produce persisted validation authority")
	require.Equal(t, "synthetic_basis", callerValidRows[0].Basis,
		"valid caller-held rows are not an input to stored-row validation")
}

func TestPrivilegeFreezeRejectsConcurrentAndChangedAuthority(t *testing.T) {
	tests := map[string]func(*privilegeTestStore){
		"concurrent rows": func(store *privilegeTestStore) {
			store.draft.Generation++
			store.draft.Rows[0].PublicDescription = "Changed stored description."
		},
		"source selection": func(store *privilegeTestStore) {
			store.draft.Withheld.Members[0].SourceSHA256 = approvalTestSHA("d")
			store.draft.Withheld.SHA256 = canonicalWithheldDigest(t, store.draft.Withheld)
		},
		"players": func(store *privilegeTestStore) {
			store.draft.Players.Revision++
			store.draft.Players.SHA256 = canonicalPlayersDigest(t, store.draft.Players)
		},
		"policy": func(store *privilegeTestStore) {
			store.draft.Policy.Name = "Changed synthetic policy"
			store.draft.Policy.SHA256 = canonicalPolicyDigest(t, store.draft.Policy)
		},
		"approval": func(store *privilegeTestStore) {
			store.draft.ApprovalEvaluation.EvaluatedAt = "2026-09-22T03:00:01.5Z"
			store.draft.ApprovalEvaluation.SHA256 = canonicalApprovalEvaluationDigest(t, *store.draft.ApprovalEvaluation)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := newPrivilegeTestStore(t)
			validation := validatePrivilegeTestStore(t, store)
			attachPrivilegeApproval(t, store, validation.Validation.InputsSHA256)
			request := freezePrivilegeTestRequest(t, store, validation)
			mutate(store)

			_, err := FreezeStoredPrivilegeLog(t.Context(), store, request)
			require.Error(t, err)
			require.Nil(t, store.receipt, "stale authority cannot partially persist a frozen receipt")
		})
	}
}

func TestPrivilegeFreezeCommitsExactStoredAuthority(t *testing.T) {
	earlyStore := newPrivilegeTestStore(t)
	earlyValidation := validatePrivilegeTestStore(t, earlyStore)
	attachPrivilegeApproval(t, earlyStore, earlyValidation.Validation.InputsSHA256)
	earlyRequest := freezePrivilegeTestRequest(t, earlyStore, earlyValidation)
	earlyRequest.FrozenAt = mustApprovalTime(t, "2026-09-22T03:00:00.5Z")
	_, err := FreezeStoredPrivilegeLog(t.Context(), earlyStore, earlyRequest)
	require.Error(t, err, "a receipt cannot freeze before its required approval evaluation")
	require.Nil(t, earlyStore.receipt)

	store := newPrivilegeTestStore(t)
	validation := validatePrivilegeTestStore(t, store)
	attachPrivilegeApproval(t, store, validation.Validation.InputsSHA256)
	request := freezePrivilegeTestRequest(t, store, validation)

	receipt, err := FreezeStoredPrivilegeLog(t.Context(), store, request)
	require.NoError(t, err)
	require.Equal(t, validation.Validation.RowsSHA256, receipt.RowsSHA256)
	require.Equal(t, validation.Validation.InputsSHA256, receipt.InputsSHA256)
	require.Equal(t, store.draft.ApprovalEvaluation.SHA256, receipt.ApprovalEvaluationSHA256)
	require.Equal(t, receipt, *store.receipt)

	retry, err := FreezeStoredPrivilegeLog(t.Context(), store, request)
	require.NoError(t, err)
	require.Equal(t, receipt, retry, "exact operation retry returns the original receipt")
	changed := request
	changed.FrozenAt = changed.FrozenAt.Add(time.Second)
	_, err = FreezeStoredPrivilegeLog(t.Context(), store, changed)
	requireApprovalProblem(t, err, documentproduction.ProblemChangedPayload)
}

func TestPrivilegeFreezeReevaluatesApprovalAtFreezeTime(t *testing.T) {
	expiredStore := newPrivilegeTestStore(t)
	expiredValidation := validatePrivilegeTestStore(t, expiredStore)
	attachPrivilegeApproval(t, expiredStore, expiredValidation.Validation.InputsSHA256)
	expiredRequest := freezePrivilegeTestRequest(t, expiredStore, expiredValidation)
	expiredRequest.FrozenAt = mustApprovalTime(t, "2026-09-23T02:00:00Z")
	_, err := FreezeStoredPrivilegeLog(t.Context(), expiredStore, expiredRequest)
	requireApprovalProblem(t, err, documentproduction.ProblemApprovalStale)
	require.Nil(t, expiredStore.receipt)

	for _, eventCase := range []struct {
		name, kind, replacementID string
	}{
		{name: "revoked", kind: documentproduction.ApprovalEventRevoke},
		{name: "superseded", kind: documentproduction.ApprovalEventSupersede, replacementID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"},
	} {
		t.Run(eventCase.name, func(t *testing.T) {
			store := newPrivilegeTestStore(t)
			validation := validatePrivilegeTestStore(t, store)
			attachPrivilegeApproval(t, store, validation.Validation.InputsSHA256)
			event, eventErr := PrepareApprovalEvent(*store.draft.ApprovalGrant, ApprovalEventRequest{
				OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", EventID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				Kind: eventCase.kind, EffectiveAt: "2026-09-22T03:00:01.5Z", ReplacementApprovalID: eventCase.replacementID,
				Reason: "Synthetic private reason.",
			})
			require.NoError(t, eventErr)
			store.draft.ApprovalEvents = []documentproduction.ApprovalEvent{event.Event}
			_, eventErr = FreezeStoredPrivilegeLog(t.Context(), store, freezePrivilegeTestRequest(t, store, validation))
			requireApprovalProblem(t, eventErr, documentproduction.ProblemApprovalStale)
			require.Nil(t, store.receipt)
		})
	}
}

func TestPrivilegeFreezeWithoutRequiredApproval(t *testing.T) {
	newStore := func(t *testing.T) *privilegeTestStore {
		t.Helper()
		store := newPrivilegeTestStore(t)
		store.draft.Policy.Approval = documentproduction.ApprovalRequirement{}
		store.draft.Policy.SHA256 = canonicalPolicyDigest(t, store.draft.Policy)
		store.draft.Withheld.PolicySHA256 = store.draft.Policy.SHA256
		store.draft.Withheld.SHA256 = canonicalWithheldDigest(t, store.draft.Withheld)
		return store
	}

	store := newStore(t)
	validation := validatePrivilegeTestStore(t, store)
	receipt, err := FreezeStoredPrivilegeLog(t.Context(), store, freezePrivilegeTestRequest(t, store, validation))
	require.NoError(t, err)
	require.Empty(t, receipt.ApprovalEvaluationSHA256)

	expectedStore := newStore(t)
	expectedValidation := validatePrivilegeTestStore(t, expectedStore)
	expectedRequest := freezePrivilegeTestRequest(t, expectedStore, expectedValidation)
	expectedRequest.ExpectedApprovalEvaluationSHA256 = approvalTestSHA("f")
	_, err = FreezeStoredPrivilegeLog(t.Context(), expectedStore, expectedRequest)
	require.Error(t, err)
	require.Nil(t, expectedStore.receipt)

	for _, storedCase := range []struct {
		name   string
		mutate func(*StoredPrivilegeLog)
	}{
		{name: "evaluation", mutate: func(stored *StoredPrivilegeLog) {
			stored.ApprovalEvaluation = &documentproduction.ApprovalEvaluation{SHA256: approvalTestSHA("e")}
		}},
		{name: "grant", mutate: func(stored *StoredPrivilegeLog) {
			stored.ApprovalGrant = &documentproduction.ApprovalGrant{}
		}},
	} {
		t.Run("stored "+storedCase.name, func(t *testing.T) {
			storedStore := newStore(t)
			storedValidation := validatePrivilegeTestStore(t, storedStore)
			storedCase.mutate(&storedStore.draft)
			_, storedErr := FreezeStoredPrivilegeLog(t.Context(), storedStore, freezePrivilegeTestRequest(t, storedStore, storedValidation))
			requireApprovalProblem(t, storedErr, documentproduction.ProblemInvalidContract)
			require.Nil(t, storedStore.receipt)
		})
	}
}

func TestPrivilegeSelectionPinsVersionsAndCorrectionsLinkNewSets(t *testing.T) {
	policy, withheld, _, _ := internalPrivilegeFixture(t)
	withoutDigest := withheld
	withoutDigest.SHA256 = ""
	prepared, err := PrepareWithheldSelection("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", withoutDigest, nil)
	require.NoError(t, err)

	changed := withoutDigest
	changed.Members = append([]documentproduction.WithheldMember(nil), withoutDigest.Members...)
	changed.Members[0].SourceVersionID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	changed.Members[0].SourceSHA256 = approvalTestSHA("b")
	changed.Members[0].Family = standalonePrivilegeFamily(changed.Members[0].SourceVersionID)
	changedPrepared, err := PrepareWithheldSelection("cccccccc-cccc-4ccc-8ccc-cccccccccccc", changed, nil)
	require.NoError(t, err)
	require.NotEqual(t, prepared.SelectionSHA256, changedPrepared.SelectionSHA256)

	produced := []redaction.Member{{ID: withoutDigest.Members[0].ID,
		SourceVersionID: withoutDigest.Members[0].SourceVersionID, Ordinal: withoutDigest.Members[0].Ordinal}}
	_, err = PrepareWithheldSelection("dddddddd-dddd-4ddd-8ddd-dddddddddddd", withoutDigest, produced)
	require.Error(t, err, "a produced occurrence cannot also be withheld")

	correction, err := PreparePrivilegeLogDraft(PrivilegeLogDraftRequest{
		OperationID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", LogID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Revision: 1,
		PredecessorLogID: withheld.ID, PredecessorReceiptSHA256: approvalTestSHA("f"),
	}, prepared.Selection, policy)
	require.NoError(t, err)
	require.Equal(t, withheld.ID, correction.PredecessorLogID)
	require.NotEmpty(t, correction.RequestSHA256)

	_, err = PreparePrivilegeLogDraft(PrivilegeLogDraftRequest{
		OperationID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", LogID: withheld.ID, Revision: 3,
		PredecessorLogID: withheld.ID, PredecessorReceiptSHA256: approvalTestSHA("f"),
	}, prepared.Selection, policy)
	require.Error(t, err, "corrections create a linked new log instead of mutating the prior set")
}

type privilegeTestStore struct {
	draft      StoredPrivilegeLog
	validation *PreparedPrivilegeLogValidation
	receipt    *documentproduction.PrivilegeLogReceipt
	operations map[string]string
}

func (s *privilegeTestStore) ValidatePrivilegeLog(_ context.Context, request PrivilegeLogValidationRequest, build PrivilegeLogValidationBuilder) (PreparedPrivilegeLogValidation, error) {
	prepared, err := build(s.draft)
	if err != nil {
		return PreparedPrivilegeLogValidation{}, err
	}
	s.validation = &prepared
	s.draft.Validation = &prepared
	s.operations[request.OperationID] = prepared.RequestSHA256
	return prepared, nil
}

func (s *privilegeTestStore) FreezePrivilegeLog(_ context.Context, request PrivilegeLogFreezeRequest, build PrivilegeLogFreezeBuilder) (documentproduction.PrivilegeLogReceipt, error) {
	if existingDigest, exists := s.operations[request.OperationID]; exists {
		requestDigest, err := privilegeLogFreezeRequestDigest(request)
		if err != nil {
			return documentproduction.PrivilegeLogReceipt{}, err
		}
		if existingDigest != requestDigest {
			return documentproduction.PrivilegeLogReceipt{}, &documentproduction.Problem{Code: documentproduction.ProblemChangedPayload, Detail: "operation payload changed"}
		}
		return *s.receipt, nil
	}
	receipt, requestDigest, err := build(s.draft)
	if err != nil {
		return documentproduction.PrivilegeLogReceipt{}, err
	}
	s.receipt = &receipt
	s.operations[request.OperationID] = requestDigest
	return receipt, nil
}

func newPrivilegeTestStore(t *testing.T) *privilegeTestStore {
	t.Helper()
	policy, withheld, players, rows := internalPrivilegeFixture(t)
	return &privilegeTestStore{draft: StoredPrivilegeLog{
		LogID: withheld.ID, Revision: withheld.Revision, Generation: 7,
		Rows: rows, Withheld: withheld, Policy: policy, Players: players,
	}, operations: make(map[string]string)}
}

func internalPrivilegeFixture(t *testing.T) (documentproduction.PolicyVersion, documentproduction.WithheldSelection, documentproduction.PlayersSnapshot, []documentproduction.PrivilegeRow) {
	t.Helper()
	policy := internalPolicyTestVersion()
	policy.Approval.MaxAgeSeconds = 24 * 60 * 60
	policy.PrivilegeLog = documentproduction.PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
		RequiredFields: []string{"date", "document_type"}, AllowedBases: []string{"synthetic_basis"}}
	policy.SHA256 = canonicalPolicyDigest(t, policy)
	withheld := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1, ID: "22222222-2222-4222-8222-222222222222",
		SetID: "33333333-3333-4333-8333-333333333333", Revision: 2, PolicySHA256: policy.SHA256,
		Members: []documentproduction.WithheldMember{{
			ID: "44444444-4444-4444-8444-444444444444", Ordinal: 1,
			SourceVersionID: "55555555-5555-4555-8555-555555555555", SourceSHA256: approvalTestSHA("1"), SourceSize: 11,
			FamilyOrder: 1, Family: standalonePrivilegeFamily("55555555-5555-4555-8555-555555555555"),
		}},
	}
	withheld.SHA256 = canonicalWithheldDigest(t, withheld)
	players := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1, ID: "66666666-6666-4666-8666-666666666666", Revision: 3,
		Players: []documentproduction.Player{{ID: "77777777-7777-4777-8777-777777777777", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: approvalTestSHA("2")}},
	}
	players.SHA256 = canonicalPlayersDigest(t, players)
	rows := []documentproduction.PrivilegeRow{{
		ID: "88888888-8888-4888-8888-888888888888", WithheldMemberID: withheld.Members[0].ID,
		FamilyOrder: 1, SourceVersionID: withheld.Members[0].SourceVersionID, Basis: "synthetic_basis",
		PublicDescription: "Synthetic message.", PrivateRationale: "Synthetic private rationale.",
		EvidenceSHA256: approvalTestSHA("3"), PersonIDs: []string{players.Players[0].ID},
		Fields: []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}, {Name: "document_type", Value: "Message"}},
	}}
	return policy, withheld, players, rows
}

func validatePrivilegeTestStore(t *testing.T, store *privilegeTestStore) PreparedPrivilegeLogValidation {
	t.Helper()
	prepared, err := ValidateStoredPrivilegeLog(t.Context(), store, PrivilegeLogValidationRequest{
		OperationID: "11111111-1111-4111-8111-111111111111", LogID: store.draft.LogID,
		Revision: store.draft.Revision, ExpectedGeneration: store.draft.Generation,
		ValidatedAt: mustApprovalTime(t, "2026-09-22T03:00:00Z"),
	})
	require.NoError(t, err)
	return prepared
}

func attachPrivilegeApproval(t *testing.T, store *privilegeTestStore, inputsSHA256 string) {
	t.Helper()
	subject := approvalTestSubject(store.draft.Policy)
	subject.SetID = store.draft.Withheld.SetID
	subject.Revision = store.draft.Withheld.Revision
	subject.WithheldSelectionSHA256 = store.draft.Withheld.SHA256
	subject.PrivilegeLogInputsSHA256 = inputsSHA256
	record := approvalTestRecord(t, store.draft.Policy, subject)
	evaluation, err := EvaluateRequiredApproval(store.draft.Policy, subject, &record.Grant, nil, mustApprovalTime(t, "2026-09-22T03:00:02Z"))
	require.NoError(t, err)
	store.draft.ApprovalSubject = &subject
	store.draft.ApprovalGrant = &record.Grant
	store.draft.ApprovalEvaluation = &evaluation
}

func freezePrivilegeTestRequest(t *testing.T, store *privilegeTestStore, validation PreparedPrivilegeLogValidation) PrivilegeLogFreezeRequest {
	t.Helper()
	approvalDigest := ""
	if store.draft.ApprovalEvaluation != nil {
		approvalDigest = store.draft.ApprovalEvaluation.SHA256
	}
	return PrivilegeLogFreezeRequest{
		OperationID: "99999999-9999-4999-8999-999999999999", LogID: store.draft.LogID,
		Revision: store.draft.Revision, ExpectedGeneration: store.draft.Generation,
		ExpectedInputsSHA256:             validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: approvalDigest,
		FrozenAt:                         mustApprovalTime(t, "2026-09-22T03:00:02Z"),
	}
}

func clonePrivilegeRows(values []documentproduction.PrivilegeRow) []documentproduction.PrivilegeRow {
	result := append([]documentproduction.PrivilegeRow(nil), values...)
	for index := range result {
		result[index].PersonIDs = append([]string(nil), values[index].PersonIDs...)
		result[index].Fields = append([]documentproduction.PrivilegeField(nil), values[index].Fields...)
	}
	return result
}

func canonicalPolicyDigest(t *testing.T, value documentproduction.PolicyVersion) string {
	t.Helper()
	value.SHA256 = ""
	_, digest, err := documentproduction.CanonicalPolicyVersion(value)
	require.NoError(t, err)
	return digest
}

func canonicalWithheldDigest(t *testing.T, value documentproduction.WithheldSelection) string {
	t.Helper()
	value.SHA256 = ""
	_, digest, err := documentproduction.CanonicalWithheldSelection(value)
	require.NoError(t, err)
	return digest
}

func canonicalPlayersDigest(t *testing.T, value documentproduction.PlayersSnapshot) string {
	t.Helper()
	value.SHA256 = ""
	_, digest, err := documentproduction.CanonicalPlayersSnapshot(value)
	require.NoError(t, err)
	return digest
}

func canonicalApprovalEvaluationDigest(t *testing.T, value documentproduction.ApprovalEvaluation) string {
	t.Helper()
	value.SHA256 = ""
	_, digest, err := documentproduction.CanonicalApprovalEvaluation(value)
	require.NoError(t, err)
	return digest
}

func standalonePrivilegeFamily(sourceVersionID string) redaction.FamilyContext {
	return redaction.FamilyContext{Kind: "standalone", RootVersionID: sourceVersionID}
}
