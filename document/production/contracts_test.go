package production

import (
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/redaction"
)

const (
	setID                        = "11111111-1111-4111-8111-111111111111"
	policyID                     = "22222222-2222-4222-8222-222222222222"
	approvalID                   = "33333333-3333-4333-8333-333333333333"
	memberOneID                  = "44444444-4444-4444-8444-444444444444"
	memberTwoID                  = "55555555-5555-4555-8555-555555555555"
	versionOneID                 = "66666666-6666-4666-8666-666666666666"
	versionTwoID                 = "77777777-7777-4777-8777-777777777777"
	fixturePrivilegeInputsSHA256 = "b40059bae1ec5998b9ae6b6b5af0e6fd54a5102e3484e00705b605d888f67bb4"
)

func TestCanonicalPolicyVersionPinsRulesRequirementsAndChangedPayload(t *testing.T) {
	policy := syntheticPolicy()
	encoded, digest, err := CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("0", 0), policy.SHA256,
		"callers do not supply a self digest to the canonical encoder")
	require.Len(t, digest, 64)

	reordered := policy
	reordered.Rules = slices.Clone(policy.Rules)
	slices.Reverse(reordered.Rules)
	reorderedBytes, reorderedDigest, err := CanonicalPolicyVersion(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes)
	require.Equal(t, digest, reorderedDigest)

	var decoded PolicyVersion
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, []string{"date-window", "family-complete"}, []string{decoded.Rules[0].ID, decoded.Rules[1].ID})

	for name, mutate := range map[string]func(*PolicyVersion){
		"rule disposition":  func(value *PolicyVersion) { value.Rules[0].Disposition = "withhold" },
		"approval required": func(value *PolicyVersion) { value.Approval = ApprovalRequirement{} },
		"log required":      func(value *PolicyVersion) { value.PrivilegeLog = PrivilegeLogRequirement{} },
		"output label":      func(value *PolicyVersion) { value.Output.ConfidentialityLabel = "SYNTHETIC CONFIDENTIAL" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := policy
			changed.Rules = slices.Clone(policy.Rules)
			mutate(&changed)
			_, changedDigest, changedErr := CanonicalPolicyVersion(changed)
			require.NoError(t, changedErr)
			require.NotEqual(t, digest, changedDigest)
		})
	}
	policy.ConflictMode = ""
	_, _, err = CanonicalPolicyVersion(policy)
	require.Error(t, err, "produce/withhold conflict behavior cannot be implicit")

	policy = syntheticPolicy()
	policy.Rules = append(policy.Rules, PolicyRule{
		ID: "missing-disposition", Kind: PolicyRuleDisposition,
		Predicate: PolicyPredicate{Field: "document.tag", Operator: PolicyOperatorPresent},
	})
	_, _, err = CanonicalPolicyVersion(policy)
	require.Error(t, err, "a disposition rule cannot be a no-op")

	policy = syntheticPolicy()
	policy.Rules = append(policy.Rules, PolicyRule{
		ID: "duplicate-equals", Kind: PolicyRuleScope, Disposition: PolicyDispositionProduce,
		Predicate: PolicyPredicate{Field: "document.scope", Operator: PolicyOperatorEquals, Values: []string{"synthetic", "synthetic"}},
	})
	_, _, err = CanonicalPolicyVersion(policy)
	require.Error(t, err, "equals predicate duplicates cannot be normalized into a valid policy")
}

func TestApprovalSubjectBindsEveryPersistedInputAndOccurrenceOrder(t *testing.T) {
	policy := syntheticPolicy()
	_, policyDigest, err := CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	subject := syntheticApprovalSubject(policyDigest)

	encoded, digest, err := CanonicalApprovalSubject(subject)
	require.NoError(t, err)
	require.Len(t, digest, 64)

	reordered := subject
	reordered.Members = []ApprovalMember{subject.Members[1], subject.Members[0]}
	reorderedBytes, reorderedDigest, err := CanonicalApprovalSubject(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes, "ordinal, not caller slice order, is authority")
	require.Equal(t, digest, reorderedDigest)

	mutations := map[string]func(*ApprovalSubject){
		"membership":           func(value *ApprovalSubject) { value.Members[0].MemberID = "abababab-abab-4bab-8bab-abababababab" },
		"source":               func(value *ApprovalSubject) { value.Members[0].SourceSHA256 = sha("1") },
		"decision":             func(value *ApprovalSubject) { value.Members[0].DecisionsSHA256 = sha("2") },
		"resolved plan":        func(value *ApprovalSubject) { value.Members[0].ResolvedSHA256 = sha("3") },
		"instructions":         func(value *ApprovalSubject) { value.InstructionsSHA256 = sha("4") },
		"recipe":               func(value *ApprovalSubject) { value.RecipeSHA256 = sha("5") },
		"output profile":       func(value *ApprovalSubject) { value.OutputProfileSHA256 = sha("6") },
		"disclosure profile":   func(value *ApprovalSubject) { value.DisclosureProfileSHA256 = sha("7") },
		"numbering policy":     func(value *ApprovalSubject) { value.NumberingPolicySHA256 = sha("8") },
		"policy version":       func(value *ApprovalSubject) { value.Policy.Version++ },
		"withheld selection":   func(value *ApprovalSubject) { value.WithheldSelectionSHA256 = sha("9") },
		"privilege log inputs": func(value *ApprovalSubject) { value.PrivilegeLogInputsSHA256 = sha("a") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := subject
			changed.Members = slices.Clone(subject.Members)
			mutate(&changed)
			_, changedDigest, changedErr := CanonicalApprovalSubject(changed)
			require.NoError(t, changedErr)
			require.NotEqual(t, digest, changedDigest)
		})
	}
}

func TestProductionGateEvidencePinBindsFactsAndPublication(t *testing.T) {
	pin := &ProductionMemberEvidencePin{
		AllowlistVersion:              PolicyFactsAllowlistV1,
		SourceMetadataGenerationID:    "70000000-0000-4000-8000-000000000001",
		SourceMetadataEvidenceSHA256:  sha("1"),
		PolicyFactsSHA256:             sha("2"),
		EmailRootVersionID:            versionOneID,
		EmailPublicationOperationID:   "70000000-0000-4000-8000-000000000002",
		EmailPublicationRequestSHA256: sha("3"),
		EmailPublicationReceiptSHA256: sha("4"),
	}
	members := []PreparedMember{
		{Member: redaction.Member{ID: memberOneID, Ordinal: 1}, EvidencePin: pin},
		{Member: redaction.Member{ID: memberTwoID, Ordinal: 2}, EvidencePin: pin},
	}
	_, digest, err := CanonicalProductionGateEvidence(members)
	require.NoError(t, err)
	require.Len(t, digest, 64)

	changed := slices.Clone(members)
	firstPin := *pin
	changed[0].EvidencePin = &firstPin
	changed[0].EvidencePin.SourceMetadataGenerationID = "70000000-0000-4000-8000-000000000003"
	_, changedDigest, err := CanonicalProductionGateEvidence(changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, changedDigest, "changing the exact fact generation must change the evidence digest")

	changed = slices.Clone(members)
	secondPin := *pin
	changed[1].EvidencePin = &secondPin
	changed[1].EvidencePin.EmailPublicationOperationID = "70000000-0000-4000-8000-000000000004"
	_, changedDigest, err = CanonicalProductionGateEvidence(changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, changedDigest, "changing the exact email publication must change the evidence digest")

	_, reorderedDigest, err := CanonicalProductionGateEvidence([]PreparedMember{members[1], members[0]})
	require.NoError(t, err, "production ordinal, not caller slice order, is authority")
	require.Equal(t, digest, reorderedDigest)
}

func TestApprovalSubjectV2RequiresGateEvidenceDigest(t *testing.T) {
	policy := syntheticPolicy()
	_, policyDigest, err := CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	subject := syntheticApprovalSubject(policyDigest)
	subject.Contract = ApprovalSubjectContractV2
	subject.GateEvidenceSHA256 = sha("9")
	_, digest, err := CanonicalApprovalSubject(subject)
	require.NoError(t, err)
	require.NotEmpty(t, digest)

	subject.GateEvidenceSHA256 = ""
	_, _, err = CanonicalApprovalSubject(subject)
	require.Error(t, err, "v2 approval subjects must bind the exact gate evidence")

	subject.Contract = ApprovalSubjectContractV1
	subject.GateEvidenceSHA256 = sha("9")
	_, _, err = CanonicalApprovalSubject(subject)
	require.Error(t, err, "v1 approval subjects cannot carry v2 gate evidence")
}

func TestApprovalExpiryRevocationSupersessionAndHistoricalReceipt(t *testing.T) {
	policy := syntheticPolicy()
	_, policyDigest, err := CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	_, subjectDigest, err := CanonicalApprovalSubject(syntheticApprovalSubject(policyDigest))
	require.NoError(t, err)
	authority := ApprovalAuthority{
		Contract: ApprovalAuthorityContractV1, Kind: ApprovalAuthorityAuthenticatedHuman,
		PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
		AuthenticatedAt: "2026-09-22T00:59:00Z", EvidenceSHA256: sha("0"),
	}
	_, authorityDigest, err := CanonicalApprovalAuthority(authority)
	require.NoError(t, err)
	grant := ApprovalGrant{
		Contract: ApprovalGrantContractV1, ID: approvalID, SubjectSHA256: subjectDigest,
		Actor: "synthetic-reviewer", AuthorityKind: ApprovalAuthorityAuthenticatedHuman,
		AuthoritySHA256: authorityDigest, Evidence: "Approved synthetic fixture.",
		GrantedAt: "2026-09-22T01:00:00Z", ExpiresAt: "2026-09-22T03:00:00Z",
	}
	_, grantDigest, err := CanonicalApprovalGrant(grant)
	require.NoError(t, err)
	grant.SHA256 = grantDigest
	callerClaim := grant
	callerClaim.AuthoritySHA256 = ""
	require.Error(t, ValidateApprovalGrant(callerClaim), "a caller-provided actor name is not approval authority")
	authority.AuthenticationMethod = "changed-synthetic-authentication"
	_, changedAuthorityDigest, err := CanonicalApprovalAuthority(authority)
	require.NoError(t, err)
	require.NotEqual(t, grant.AuthoritySHA256, changedAuthorityDigest)

	_, err = EvaluateApproval(grant, nil, subjectDigest, mustTime(t, "2026-09-22T00:59:59Z"))
	requireProblemCode(t, err, ProblemInvalidContract)
	atGrant, err := EvaluateApproval(grant, nil, subjectDigest, mustTime(t, grant.GrantedAt))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateCurrent, atGrant.State)

	current, err := EvaluateApproval(grant, nil, subjectDigest, mustTime(t, "2026-09-22T02:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateCurrent, current.State)
	frozen, err := EvaluateApproval(grant, nil, subjectDigest, mustTime(t, "2026-09-22T01:30:00Z"))
	require.NoError(t, err)
	require.NotEqual(t, frozen.SHA256, current.SHA256, "freeze and admission evaluations bind different times")
	require.NoError(t, ApprovalGateProblem(true, &frozen, subjectDigest))
	require.NoError(t, ApprovalGateProblem(true, &current, subjectDigest))
	requireProblemCode(t, ApprovalGateProblem(true, &current, sha("f")), ProblemApprovalStale)
	corruptEvaluation := current
	corruptEvaluation.EventsSHA256 = sha("e")
	corruptErr := ApprovalGateProblem(true, &corruptEvaluation, subjectDigest)
	requireProblemCode(t, corruptErr, ProblemChangedPayload)
	corruptProblem := &Problem{}
	require.ErrorAs(t, corruptErr, &corruptProblem)
	require.Equal(t, subjectDigest, corruptProblem.SubjectID)

	expired, err := EvaluateApproval(grant, nil, subjectDigest, mustTime(t, "2026-09-22T04:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateExpired, expired.State)
	requireProblemCode(t, ApprovalGateProblem(true, &expired, subjectDigest), ProblemApprovalStale)

	revocation := ApprovalEvent{Contract: ApprovalEventContractV1,
		ID: "88888888-8888-4888-8888-888888888888", ApprovalID: approvalID,
		Kind: ApprovalEventRevoke, EffectiveAt: "2026-09-22T02:30:00Z", Reason: "Synthetic revocation."}
	revoked, err := EvaluateApproval(grant, []ApprovalEvent{revocation}, subjectDigest, mustTime(t, "2026-09-22T02:45:00Z"))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateRevoked, revoked.State)
	requireProblemCode(t, ApprovalGateProblem(true, &revoked, subjectDigest), ProblemApprovalStale)

	supersession := ApprovalEvent{Contract: ApprovalEventContractV1,
		ID: "99999999-9999-4999-8999-999999999999", ApprovalID: approvalID,
		Kind: ApprovalEventSupersede, EffectiveAt: "2026-09-22T02:15:00Z",
		ReplacementApprovalID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	superseded, err := EvaluateApproval(grant, []ApprovalEvent{supersession}, subjectDigest, mustTime(t, "2026-09-22T02:20:00Z"))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateSuperseded, superseded.State)

	wrongSubject, err := EvaluateApproval(grant, nil, sha("f"), mustTime(t, "2026-09-22T02:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateStaleSubject, wrongSubject.State)

	receipt := syntheticProductionReceipt(current)
	_, receiptDigest, err := CanonicalProductionReceipt(receipt)
	require.NoError(t, err)
	receipt.SHA256 = receiptDigest
	require.NoError(t, ValidateProductionReceipt(receipt),
		"a receipt binds the admission-time current evaluation; later events do not rewrite history")
	_, sameReceiptDigest, err := CanonicalProductionReceipt(receipt)
	require.NoError(t, err)
	require.Equal(t, receiptDigest, sameReceiptDigest)
}

func TestApprovalEventsUseChronologicalOrderAcrossTimestampPrecision(t *testing.T) {
	policy := syntheticPolicy()
	_, policyDigest, err := CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	_, subjectDigest, err := CanonicalApprovalSubject(syntheticApprovalSubject(policyDigest))
	require.NoError(t, err)
	authority := ApprovalAuthority{Contract: ApprovalAuthorityContractV1, Kind: ApprovalAuthorityAuthenticatedHuman,
		PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
		AuthenticatedAt: "2026-09-22T00:59:00Z", EvidenceSHA256: sha("0")}
	_, authorityDigest, err := CanonicalApprovalAuthority(authority)
	require.NoError(t, err)
	grant := ApprovalGrant{Contract: ApprovalGrantContractV1, ID: approvalID, SubjectSHA256: subjectDigest,
		Actor: "synthetic-reviewer", AuthorityKind: ApprovalAuthorityAuthenticatedHuman, AuthoritySHA256: authorityDigest,
		GrantedAt: "2026-09-22T01:00:00Z"}
	_, grant.SHA256, err = CanonicalApprovalGrant(grant)
	require.NoError(t, err)
	events := []ApprovalEvent{
		{Contract: ApprovalEventContractV1, ID: "88888888-8888-4888-8888-888888888888", ApprovalID: approvalID,
			Kind: ApprovalEventRevoke, EffectiveAt: "2026-09-22T02:30:00Z"},
		{Contract: ApprovalEventContractV1, ID: "99999999-9999-4999-8999-999999999999", ApprovalID: approvalID,
			Kind: ApprovalEventSupersede, EffectiveAt: "2026-09-22T02:30:00.5Z", ReplacementApprovalID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
	}
	evaluation, err := EvaluateApproval(grant, events, subjectDigest, mustTime(t, "2026-09-22T02:31:00Z"))
	require.NoError(t, err)
	require.Equal(t, ApprovalStateRevoked, evaluation.State, "the chronologically first event owns the lifecycle state")
}

func TestWithheldSelectionIsExplicitVersionPinnedAndDisjoint(t *testing.T) {
	selection := WithheldSelection{
		Contract: WithheldSelectionContractV1, ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		SetID: setID, Revision: 1, PolicySHA256: sha("a"),
		Members: []WithheldMember{
			{ID: memberTwoID, Ordinal: 2, SourceVersionID: versionTwoID, SourceSHA256: sha("b"), SourceSize: 22, FamilyOrder: 2, Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionTwoID}},
			{ID: memberOneID, Ordinal: 1, SourceVersionID: versionOneID, SourceSHA256: sha("c"), SourceSize: 11, FamilyOrder: 1, Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionOneID}},
		},
	}
	encoded, digest, err := CanonicalWithheldSelection(selection)
	require.NoError(t, err)
	require.Len(t, digest, 64)
	var decoded WithheldSelection
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, memberOneID, decoded.Members[0].ID)

	changed := selection
	changed.Members = slices.Clone(selection.Members)
	changed.Members[0].SourceVersionID = versionOneID
	changed.Members[0].Family.RootVersionID = versionOneID
	_, changedDigest, err := CanonicalWithheldSelection(changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, changedDigest)

	produced := []redaction.Member{{ID: memberOneID, SourceVersionID: versionOneID, Ordinal: 1}}
	require.Error(t, ValidateSelectionPartition(produced, selection),
		"one occurrence cannot be silently both produced and withheld")
	storedSelection := selection
	storedSelection.SHA256 = digest
	require.Error(t, ValidateSelectionPartition(produced, storedSelection), "stored canonical selections use the same partition gate")
	produced[0].ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	require.NoError(t, ValidateSelectionPartition(produced, selection))
}

func TestFrozenPrivilegeLogDoesNotDependOnFutureAssignedNumbers(t *testing.T) {
	rows := []PrivilegeRow{
		{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", WithheldMemberID: memberTwoID, FamilyOrder: 2,
			SourceVersionID: versionTwoID, Basis: "synthetic_basis", PublicDescription: "Synthetic description two.",
			PrivateRationale: "Synthetic private rationale two.", EvidenceSHA256: sha("d"),
			PersonIDs: []string{memberTwoID}, Fields: []PrivilegeField{{Name: "topic", Value: "Synthetic topic two"}}},
		{ID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", WithheldMemberID: memberOneID, FamilyOrder: 1,
			SourceVersionID: versionOneID, Basis: "synthetic_basis", PublicDescription: "Synthetic description one.",
			PrivateRationale: "Synthetic private rationale one.", EvidenceSHA256: sha("e"),
			PersonIDs: []string{memberOneID}, Fields: []PrivilegeField{{Name: "topic", Value: "Synthetic topic one"}}},
	}
	duplicatePeople := slices.Clone(rows)
	duplicatePeople[0].PersonIDs = append(slices.Clone(duplicatePeople[0].PersonIDs), duplicatePeople[0].PersonIDs[0])
	_, _, err := CanonicalPrivilegeRows(duplicatePeople)
	require.Error(t, err)
	input := PrivilegeLogFreezeInput{
		LogID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Revision: 3,
		WithheldSelectionSHA256: sha("1"), PolicySHA256: sha("2"), PlayersSHA256: sha("3"),
		ValidatedAt: "2026-09-22T02:00:00Z", Rows: rows,
		ApprovalEvaluationSHA256: sha("4"), FrozenAt: "2026-09-22T02:00:01Z",
	}
	_, inputsDigest, err := CanonicalPrivilegeLogInputs(input.PrivilegeLogValidationInput)
	require.NoError(t, err)
	receipt, err := FreezePrivilegeLog(input)
	require.NoError(t, err)
	require.Equal(t, PrivilegeLogStateFrozen, receipt.State)
	require.NotEmpty(t, receipt.RowsSHA256)
	require.NotEmpty(t, receipt.InputsSHA256)
	require.Equal(t, inputsDigest, receipt.InputsSHA256)
	require.NoError(t, PrivilegeLogGateProblem(true, &receipt, input.PolicySHA256, input.WithheldSelectionSHA256,
		input.PlayersSHA256, input.ApprovalEvaluationSHA256, inputsDigest))
	requireProblemCode(t, PrivilegeLogGateProblem(true, &receipt, input.PolicySHA256, input.WithheldSelectionSHA256,
		input.PlayersSHA256, input.ApprovalEvaluationSHA256, sha("0")), ProblemPrivilegeLogStale)
	corruptReceipt := receipt
	corruptReceipt.RowsSHA256 = sha("f")
	corruptErr := PrivilegeLogGateProblem(true, &corruptReceipt, input.PolicySHA256, input.WithheldSelectionSHA256,
		input.PlayersSHA256, input.ApprovalEvaluationSHA256, inputsDigest)
	requireProblemCode(t, corruptErr, ProblemChangedPayload)
	corruptProblem := &Problem{}
	require.ErrorAs(t, corruptErr, &corruptProblem)
	require.Equal(t, receipt.LogID, corruptProblem.SubjectID)

	changedApproval := input
	changedApproval.ApprovalEvaluationSHA256 = sha("5")
	_, sameInputsDigest, err := CanonicalPrivilegeLogInputs(changedApproval.PrivilegeLogValidationInput)
	require.NoError(t, err)
	require.Equal(t, inputsDigest, sameInputsDigest, "approval covers pre-approval log inputs without a digest cycle")
	changedApprovalReceipt, err := FreezePrivilegeLog(changedApproval)
	require.NoError(t, err)
	require.NotEqual(t, receipt.SHA256, changedApprovalReceipt.SHA256, "the frozen receipt binds the resulting evaluation")
	changedRows := input
	changedRows.Rows = slices.Clone(input.Rows)
	changedRows.Rows[0].PublicDescription = "Changed synthetic description."
	_, changedInputsDigest, err := CanonicalPrivilegeLogInputs(changedRows.PrivilegeLogValidationInput)
	require.NoError(t, err)
	require.NotEqual(t, inputsDigest, changedInputsDigest, "approval binds the exact validated rows")

	public := PublicPrivilegeRows(rows)
	require.Equal(t, []string{memberOneID, memberTwoID}, []string{public[0].WithheldMemberID, public[1].WithheldMemberID})
	encodedPublic, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encodedPublic), "private rationale")
	require.NotContains(t, string(encodedPublic), sha("d"))

	attachment := PrivilegeLogAttachmentReceipt{
		Contract: PrivilegeLogAttachmentContractV1,
		ID:       "12121212-1212-4212-8212-121212121212", PrivilegeLogReceiptSHA256: receipt.SHA256,
		ProductionReceiptSHA256: sha("5"),
		References:              []PrivilegeOutputReference{{WithheldMemberID: memberOneID, AssignedNumber: "SYN000001", ArtifactSHA256: sha("6")}},
		CreatedAt:               "2026-09-22T03:00:00Z",
	}
	_, attachmentDigest, err := CanonicalPrivilegeLogAttachment(attachment)
	require.NoError(t, err)
	require.NotEqual(t, receipt.SHA256, attachmentDigest)
	require.Equal(t, receipt.SHA256, attachment.PrivilegeLogReceiptSHA256,
		"future number references attach to, and never mutate, the frozen log receipt")
}

func TestOptionalPrivilegeCollectionsNormalizeNilToEmpty(t *testing.T) {
	row := PrivilegeRow{
		ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", WithheldMemberID: memberTwoID, FamilyOrder: 2,
		SourceVersionID: versionTwoID, Basis: "synthetic_basis", PublicDescription: "Synthetic description.",
		EvidenceSHA256: sha("d"), PersonIDs: []string{memberTwoID},
	}
	nilRowsBytes, nilRowsDigest, err := CanonicalPrivilegeRows([]PrivilegeRow{row})
	require.NoError(t, err)
	row.Fields = []PrivilegeField{}
	emptyRowsBytes, emptyRowsDigest, err := CanonicalPrivilegeRows([]PrivilegeRow{row})
	require.NoError(t, err)
	require.Equal(t, nilRowsBytes, emptyRowsBytes)
	require.Equal(t, nilRowsDigest, emptyRowsDigest)
	require.Contains(t, string(nilRowsBytes), `"fields":[]`)

	attachment := PrivilegeLogAttachmentReceipt{
		Contract: PrivilegeLogAttachmentContractV1,
		ID:       "12121212-1212-4212-8212-121212121212", PrivilegeLogReceiptSHA256: sha("1"),
		ProductionReceiptSHA256: sha("2"), CreatedAt: "2026-09-22T03:00:00Z",
	}
	nilReferencesBytes, nilReferencesDigest, err := CanonicalPrivilegeLogAttachment(attachment)
	require.NoError(t, err)
	attachment.References = []PrivilegeOutputReference{}
	emptyReferencesBytes, emptyReferencesDigest, err := CanonicalPrivilegeLogAttachment(attachment)
	require.NoError(t, err)
	require.Equal(t, nilReferencesBytes, emptyReferencesBytes)
	require.Equal(t, nilReferencesDigest, emptyReferencesDigest)
	require.Contains(t, string(nilReferencesBytes), `"references":[]`)
}

func TestPlayersAndPrivatePrivilegeFieldsAreCanonicalButNotPublic(t *testing.T) {
	players := PlayersSnapshot{
		Contract: PlayersSnapshotContractV1, ID: "10101010-1010-4010-8010-101010101010", Revision: 2,
		Players: []Player{
			{ID: memberTwoID, DisplayName: "Synthetic Person Two", Aliases: []string{"Two, Synthetic", "Person Two"}, EvidenceSHA256: sha("1")},
			{ID: memberOneID, DisplayName: "Synthetic Person One", Aliases: []string{"Person One"}, EvidenceSHA256: sha("2")},
		},
	}
	encoded, digest, err := CanonicalPlayersSnapshot(players)
	require.NoError(t, err)
	require.Len(t, digest, 64)

	reordered := players
	reordered.Players = []Player{players.Players[1], players.Players[0]}
	require.Equal(t, memberTwoID, reordered.Players[1].ID)
	originalAliasOrder := slices.Clone(reordered.Players[1].Aliases)
	reordered.Players[1].Aliases = slices.Clone(reordered.Players[1].Aliases)
	slices.Reverse(reordered.Players[1].Aliases)
	require.NotEqual(t, originalAliasOrder, reordered.Players[1].Aliases)
	reorderedBytes, reorderedDigest, err := CanonicalPlayersSnapshot(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes)
	require.Equal(t, digest, reorderedDigest)

	players.SHA256 = digest
	require.NoError(t, ValidatePlayersSnapshot(players))
	players.Players[0].DisplayName = "Changed synthetic person"
	requireProblemCode(t, ValidatePlayersSnapshot(players), ProblemChangedPayload)
}

func TestArtifactManifestUsesOccurrencePageRoleOrderAndDetectsChangedPayload(t *testing.T) {
	manifest := ArtifactManifest{Contract: ArtifactManifestContractV1, Artifacts: []Artifact{
		{ID: "13131313-1313-4313-8313-131313131313", MemberID: memberTwoID, MemberOrdinal: 2, Role: ArtifactRoleRedactedPDF, Path: "VOL001/SYN000002.pdf", SHA256: sha("1"), Size: 20, MediaType: "application/pdf", Volume: "VOL001"},
		{ID: "14141414-1414-4414-8414-141414141414", MemberID: memberOneID, MemberOrdinal: 1, Page: 1, Role: ArtifactRoleRedactedPage, Path: "VOL001/SYN000001.png", SHA256: sha("2"), Size: 10, MediaType: "image/png", Volume: "VOL001"},
		{ID: "15151515-1515-4515-8515-151515151515", MemberID: memberOneID, MemberOrdinal: 1, Page: 0, Role: ArtifactRoleRedactedPDF, Path: "VOL001/SYN000001.pdf", SHA256: sha("3"), Size: 30, MediaType: "application/pdf", Volume: "VOL001"},
	}}
	encoded, digest, err := CanonicalArtifactManifest(manifest)
	require.NoError(t, err)
	require.Len(t, digest, 64)

	reordered := manifest
	reordered.Artifacts = slices.Clone(manifest.Artifacts)
	slices.Reverse(reordered.Artifacts)
	reorderedBytes, reorderedDigest, err := CanonicalArtifactManifest(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes)
	require.Equal(t, digest, reorderedDigest)

	changed := manifest
	changed.Artifacts = slices.Clone(manifest.Artifacts)
	changed.Artifacts[0].SHA256 = sha("4")
	_, changedDigest, err := CanonicalArtifactManifest(changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, changedDigest)

	for _, unsafePath := range []string{"..", "../outside.pdf", "C:/outside.pdf", "C:outside.pdf"} {
		unsafe := manifest
		unsafe.Artifacts = slices.Clone(manifest.Artifacts)
		unsafe.Artifacts[0].Path = unsafePath
		_, _, err := CanonicalArtifactManifest(unsafe)
		require.Error(t, err, unsafePath)
	}
}

func TestPreparedInputReceiptBindsOptionalAuthoritiesAndDetectsChangedPayload(t *testing.T) {
	receipt := PreparedInputReceipt{
		Contract: PreparedInputContractV1,
		ID:       "17171717-1717-4717-8717-171717171717", ApprovalSubjectSHA256: sha("1"),
		ApprovalEvaluationSHA256: sha("2"), WithheldSelectionSHA256: sha("3"),
		PrivilegeLogReceiptSHA256: sha("4"), GateResultsSHA256: sha("5"),
		CreatedAt: "2026-09-22T03:30:00Z",
	}
	_, digest, err := CanonicalPreparedInputReceipt(receipt)
	require.NoError(t, err)
	receipt.SHA256 = digest
	require.NoError(t, ValidatePreparedInputReceipt(receipt))

	receipt.GateResultsSHA256 = sha("6")
	requireProblemCode(t, ValidatePreparedInputReceipt(receipt), ProblemChangedPayload)
}

func TestNumberingProvenanceRetentionAndReproductionReceiptsAreImmutable(t *testing.T) {
	reservation := NumberReservation{
		Contract: NumberReservationContractV1, Authority: "synthetic-numbering-ledger/v1",
		ID: "18181818-1818-4818-8818-181818181818", OperationID: "19191919-1919-4919-8919-191919191919",
		RevisionSHA256: sha("1"), State: "reserved",
		Numbers: []AssignedNumber{
			{MemberID: memberTwoID, MemberOrdinal: 2, Page: 1, Text: "SYN000002"},
			{MemberID: memberOneID, MemberOrdinal: 1, Page: 1, Text: "SYN000001"},
		},
	}
	encoded, digest, err := CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	reordered := reservation
	reordered.Numbers = []AssignedNumber{reservation.Numbers[1], reservation.Numbers[0]}
	reorderedBytes, reorderedDigest, err := CanonicalNumberReservation(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes)
	require.Equal(t, digest, reorderedDigest)
	reservation.SHA256 = digest
	require.NoError(t, ValidateNumberReservation(reservation))

	provenance := ArtifactProvenanceReceipt{
		Contract: ArtifactProvenanceContractV1, ID: "20202020-2020-4020-8020-202020202020",
		ProductionReceiptSHA256: sha("2"), ArtifactManifestSHA256: sha("3"), CreatedAt: "2026-09-22T04:00:00Z",
		Entries: []ArtifactProvenance{
			{ArtifactID: "21212121-2121-4121-8121-212121212121", ArtifactSHA256: sha("4"), SourceVersionID: versionTwoID, MemberID: memberTwoID, MemberOrdinal: 2, Page: 1, Volume: "VOL002"},
			{ArtifactID: "23232323-2323-4323-8323-232323232323", ArtifactSHA256: sha("5"), SourceVersionID: versionOneID, MemberID: memberOneID, MemberOrdinal: 1, Volume: "VOL001"},
		},
	}
	_, provenanceDigest, err := CanonicalArtifactProvenanceReceipt(provenance)
	require.NoError(t, err)
	provenance.SHA256 = provenanceDigest
	require.NoError(t, ValidateArtifactProvenanceReceipt(provenance))
	provenance.Entries[0].SourceVersionID = versionOneID
	requireProblemCode(t, ValidateArtifactProvenanceReceipt(provenance), ProblemChangedPayload)

	retention := RetentionReceipt{
		Contract: RetentionReceiptContractV1, ID: "24242424-2424-4424-8424-242424242424",
		ProductionReceiptSHA256: sha("6"), ArtifactManifestSHA256: sha("7"), MetadataSHA256: sha("8"),
		BlobRootsSHA256: sha("9"), AuditSHA256: sha("a"), RetainedAt: "2026-09-22T04:01:00Z",
	}
	_, retentionDigest, err := CanonicalRetentionReceipt(retention)
	require.NoError(t, err)
	retention.SHA256 = retentionDigest
	require.NoError(t, ValidateRetentionReceipt(retention))

	request := ReproductionRequest{
		Contract: ReproductionRequestContractV1, OperationID: "25252525-2525-4525-8525-252525252525",
		OriginalProductionReceiptSHA256: sha("b"), ArtifactIDs: []string{provenance.Entries[1].ArtifactID, provenance.Entries[0].ArtifactID},
		DeliveryPolicySHA256: sha("c"),
	}
	requestBytes, requestDigest, err := CanonicalReproductionRequest(request)
	require.NoError(t, err)
	slices.Reverse(request.ArtifactIDs)
	reorderedRequestBytes, reorderedRequestDigest, err := CanonicalReproductionRequest(request)
	require.NoError(t, err)
	require.Equal(t, requestBytes, reorderedRequestBytes)
	require.Equal(t, requestDigest, reorderedRequestDigest)

	reproduction := ReproductionReceipt{
		Contract: ReproductionReceiptContractV1, ID: "26262626-2626-4626-8626-262626262626",
		OriginalProductionReceiptSHA256: sha("d"), OriginalNumberReservationSHA256: reservation.SHA256,
		ArtifactManifestSHA256: sha("e"), PackageQCSHA256: sha("f"), DeliveryPolicySHA256: sha("0"),
		NumberAllocationCount: 0, CreatedAt: "2026-09-22T04:02:00Z",
	}
	_, reproductionDigest, err := CanonicalReproductionReceipt(reproduction)
	require.NoError(t, err)
	reproduction.SHA256 = reproductionDigest
	require.NoError(t, ValidateReproductionReceipt(reproduction))
	reproduction.NumberAllocationCount = 1
	require.Error(t, ValidateReproductionReceipt(reproduction), "reproduction cannot allocate new numbers")
}

func TestSurfaceAndStorageContractsAreFrozenUniqueAndBounded(t *testing.T) {
	operations := SurfaceOperations()
	require.NotEmpty(t, operations)
	seenOperations, seenTools := map[string]bool{}, map[string]bool{}
	for _, operation := range operations {
		require.False(t, seenOperations[operation.OperationID], operation.OperationID)
		seenOperations[operation.OperationID] = true
		require.True(t, strings.HasPrefix(operation.Path, "/api/v1/"), operation.Path)
		require.NotContains(t, operation.Transport, "internal/client")
		if operation.Tool != "" {
			require.False(t, seenTools[operation.Tool], operation.Tool)
			seenTools[operation.Tool] = true
			require.True(t, strings.HasPrefix(operation.Tool, "production_"), operation.Tool)
		}
	}
	require.Contains(t, seenOperations, "createProductionPolicyVersion")
	require.Contains(t, seenOperations, "recordProductionApproval")
	require.Contains(t, seenOperations, "freezeProductionPrivilegeLog")
	require.Contains(t, seenOperations, "readProductionRetention")
	require.Contains(t, seenOperations, "createProductionReproduction")

	storage := StorageContracts()
	require.NotEmpty(t, storage)
	seenKinds := map[string]bool{}
	for _, record := range storage {
		require.False(t, seenKinds[record.RecordKind], record.RecordKind)
		seenKinds[record.RecordKind] = true
		require.NotEmpty(t, record.Writer)
		require.NotEmpty(t, record.Mutability)
	}
	require.Equal(t, StorageWriterCoordinator, storage[0].Writer)

	missing := ApprovalGateProblem(true, nil, sha("1"))
	requireProblemCode(t, missing, ProblemApprovalRequired)
	problem := &Problem{}
	ok := errors.As(missing, &problem)
	require.True(t, ok)
	require.LessOrEqual(t, len(problem.Detail), MaxProblemDetailBytes)
	require.LessOrEqual(t, len(operations), MaxSurfaceOperations)

	conflicts := PolicyUnsatisfiedProblem([]string{"member-b", "member-a"})
	requireProblemCode(t, conflicts, ProblemPolicyUnsatisfied)
	conflictProblem := &Problem{}
	require.ErrorAs(t, conflicts, &conflictProblem)
	require.Equal(t, []string{"member-a", "member-b"}, conflictProblem.IDs)
	require.NoError(t, ValidateProblem(*conflictProblem))
	requireProblemCode(t, PrivilegeLogGateProblem(true, nil, sha("1"), sha("2"), sha("3"), "", sha("4")), ProblemPrivilegeLogRequired)
}

func TestBoundedProblemDetailPreservesUTF8(t *testing.T) {
	detail := boundedDetail(strings.Repeat("a", MaxProblemDetailBytes-1) + "é")
	require.LessOrEqual(t, len(detail), MaxProblemDetailBytes)
	require.True(t, utf8.ValidString(detail))
	require.NoError(t, ValidateProblem(Problem{Code: ProblemInvalidContract, Detail: detail}))
}

func TestRedactionDraftPinsResolvedPolicySelection(t *testing.T) {
	create := redaction.CreateRequest{
		OperationID: "27272727-2727-4727-8727-272727272727", Name: "Synthetic policy selection",
		Instructions: "Use only synthetic facts.", PolicyID: policyID, PolicyVersion: 4,
	}
	require.NoError(t, redaction.ValidateCreateRequest(create))
	create.PolicyVersion = 0
	require.Error(t, redaction.ValidateCreateRequest(create), "policy ID and version are one request authority")

	change := redaction.Change{Kind: "policy", PolicyID: policyID, PolicyVersion: 4}
	require.NoError(t, redaction.ValidateChange(change))
	raw, err := json.Marshal(change)
	require.NoError(t, err)
	require.JSONEq(t, `{"kind":"policy","policy_id":"22222222-2222-4222-8222-222222222222","policy_version":4}`, string(raw))

	draft := redaction.Draft{
		SetID: setID, Revision: 1, ETag: 1,
		InstructionsSHA256: sha("1"), MemberHash: sha("2"), DecisionsSHA256: sha("3"),
		RecipeID: redaction.DefaultRecipeID, RecipeSHA256: sha("4"),
		ProfileID: redaction.DefaultOutputProfileID, ProfileSHA256: sha("5"),
		DisclosureProfileID: redaction.DefaultDisclosureProfileID, DisclosureProfileSHA256: sha("6"),
		NumberingRecipeID: redaction.BatesNumberingRecipeID, NumberingRecipeSHA256: sha("7"),
		Policy: redaction.PolicySelection{PolicyID: policyID, Version: 4, PolicySHA256: sha("8")}, State: "draft",
	}
	require.NoError(t, redaction.ValidateDraft(draft))
	draft.Policy.PolicySHA256 = ""
	require.Error(t, redaction.ValidateDraft(draft), "persisted drafts cannot retain unresolved policy requests")
}

func syntheticPolicy() PolicyVersion {
	return PolicyVersion{
		Contract: PolicyContractV1, ID: policyID, Version: 4, Name: "Synthetic production policy",
		CreatedAt: "2026-09-22T00:00:00Z",
		Rules: []PolicyRule{
			{ID: "family-complete", Kind: PolicyRuleFamily, Predicate: PolicyPredicate{Field: "family.kind", Operator: PolicyOperatorOneOf, Values: []string{"email"}}, FamilyMode: PolicyFamilyComplete, Disposition: PolicyDispositionProduce},
			{ID: "date-window", Kind: PolicyRuleDate, Predicate: PolicyPredicate{Field: "document.date", Operator: PolicyOperatorDateBetween, From: "2026-01-01", Through: "2026-12-31"}, Disposition: PolicyDispositionProduce},
		},
		Output:   PolicyOutput{ConfidentialityLabel: "SYNTHETIC", EndorsementKind: "legend", EndorsementText: "Synthetic output"},
		Approval: ApprovalRequirement{Required: true, EvidenceRequired: true, MaxAgeSeconds: 3600},
		PrivilegeLog: PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
			RequiredFields: []string{"basis", "public_description"}, AllowedBases: []string{"synthetic_basis"}},
		ConflictMode: PolicyConflictReject,
	}
}

func syntheticApprovalSubject(policyDigest string) ApprovalSubject {
	return ApprovalSubject{
		Contract: ApprovalSubjectContractV1, SetID: setID, Revision: 7,
		InstructionsSHA256: sha("a"), RecipeSHA256: sha("b"), OutputProfileSHA256: sha("c"),
		DisclosureProfileSHA256: sha("d"), NumberingPolicySHA256: sha("e"),
		Policy:                  PolicySelection{PolicyID: policyID, Version: 4, PolicySHA256: policyDigest},
		WithheldSelectionSHA256: sha("1"), PrivilegeLogInputsSHA256: fixturePrivilegeInputsSHA256,
		Members: []ApprovalMember{
			{MemberID: memberTwoID, Ordinal: 2, SourceVersionID: versionTwoID, SourceSHA256: sha("3"), SourceSize: 22, PDFSHA256: sha("4"), PageInventorySHA256: sha("5"), MapSHA256: sha("6"), DecisionsSHA256: sha("7"), ResolvedSHA256: sha("8")},
			{MemberID: memberOneID, Ordinal: 1, SourceVersionID: versionOneID, SourceSHA256: sha("9"), SourceSize: 11, PDFSHA256: sha("a"), PageInventorySHA256: sha("b"), MapSHA256: sha("c"), DecisionsSHA256: sha("d"), ResolvedSHA256: sha("e")},
		},
	}
}

func syntheticProductionReceipt(evaluation ApprovalEvaluation) ProductionReceipt {
	return ProductionReceipt{
		Contract: ProductionReceiptContractV1,
		ID:       "16161616-1616-4616-8616-161616161616", JobID: "17171717-1717-4717-8717-171717171717",
		SetID: setID, Revision: 7, RevisionSHA256: sha("1"), PreparedInputSHA256: sha("0"), ApprovalEvaluationSHA256: evaluation.SHA256,
		PolicySHA256: sha("2"), WithheldSelectionSHA256: sha("3"), PrivilegeLogReceiptSHA256: sha("4"),
		NumberReservationSHA256: sha("5"), LayoutSHA256: sha("6"), EndorsementsSHA256: sha("7"),
		ArtifactManifestSHA256: sha("8"), CreatedAt: "2026-09-22T02:05:00Z",
	}
}

func requireProblemCode(t *testing.T, err error, code ProblemCode) {
	t.Helper()
	require.Error(t, err)
	problem := &Problem{}
	ok := errors.As(err, &problem)
	require.True(t, ok)
	require.Equal(t, code, problem.Code)
}

func requireInvalidContractDetail(t *testing.T, err error, detail string) {
	t.Helper()
	requireProblemCode(t, err, ProblemInvalidContract)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, detail, problem.Detail)
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

func sha(char string) string { return strings.Repeat(char, 64) }
