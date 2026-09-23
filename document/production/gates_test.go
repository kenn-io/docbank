package production

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestGateResultsCanonicalizeEvidenceAndRejectRequiredNotChecked(t *testing.T) {
	results := GateResults{
		Contract:       GateResultsContractV1,
		PreparedSHA256: sha("1"),
		EvaluatedAt:    "2026-09-22T16:00:00Z",
		Checks:         passingSyntheticGateChecks(),
	}
	encoded, digest, err := CanonicalGateResults(results)
	require.NoError(t, err)

	reordered := results
	reordered.Checks = slices.Clone(results.Checks)
	slices.Reverse(reordered.Checks)
	reordered.Checks[0].Evidence = slices.Clone(reordered.Checks[0].Evidence)
	reorderedBytes, reorderedDigest, err := CanonicalGateResults(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes)
	require.Equal(t, digest, reorderedDigest)

	results.SHA256 = digest
	require.NoError(t, ValidateGateResults(results))
	results.SHA256 = ""
	results.Checks[0].State = GateStateNotChecked
	_, _, err = CanonicalGateResults(results)
	require.Error(t, err)
}

func TestPreparedProductionDigestPinsStoredOccurrenceAndEmptyDecisions(t *testing.T) {
	prepared := syntheticPreparedProductionContract()
	encoded, digest, err := CanonicalPreparedProduction(prepared)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	prepared.SHA256 = digest
	require.NoError(t, ValidatePreparedProduction(prepared))

	changed := prepared
	changed.SHA256 = ""
	changed.Members = slices.Clone(prepared.Members)
	changed.Members[0].Member.SourceVersionID = versionTwoID
	_, _, err = CanonicalPreparedProduction(changed)
	require.Error(t, err, "source substitution must invalidate the sealed occurrence")

	changed = prepared
	changed.SHA256 = ""
	changed.Members = slices.Clone(prepared.Members)
	changed.Members[0].Decisions = []redaction.Decision{{
		ID: memberTwoID, MemberID: memberOneID, Action: "keep",
		Selector: redaction.Selector{Kind: "page", MapSHA256: changed.Members[0].Member.MapSHA256, Pages: []int{1}},
	}}
	_, changed.Members[0].DecisionsSHA256, err = redaction.CanonicalDecisions(changed.Members[0].Decisions)
	require.NoError(t, err)
	_, _, err = CanonicalPreparedProduction(changed)
	require.Error(t, err, "empty-to-nonempty decisions must invalidate the sealed aggregate")
}

func TestPreparedProductionV2BindsGateEvidencePins(t *testing.T) {
	prepared := syntheticPreparedProductionContract()
	prepared.Contract = PreparedProductionContractV2
	prepared.Members = slices.Clone(prepared.Members)
	for index := range prepared.Members {
		_, factsSHA256, err := CanonicalPolicyMemberFacts(prepared.Members[index].Facts)
		require.NoError(t, err)
		prepared.Members[index].EvidencePin = &ProductionMemberEvidencePin{
			AllowlistVersion:             PolicyFactsAllowlistV1,
			SourceMetadataGenerationID:   "70000000-0000-4000-8000-000000000001",
			SourceMetadataEvidenceSHA256: sha("1"), PolicyFactsSHA256: factsSHA256,
		}
	}
	_, evidenceSHA256, err := CanonicalProductionGateEvidence(prepared.Members)
	require.NoError(t, err)
	prepared.ApprovalSubject.Contract = ApprovalSubjectContractV2
	prepared.ApprovalSubject.GateEvidenceSHA256 = evidenceSHA256
	_, digest, err := CanonicalPreparedProduction(prepared)
	require.NoError(t, err)
	prepared.SHA256 = digest
	require.NoError(t, ValidatePreparedProduction(prepared))

	changed := prepared
	changed.SHA256 = ""
	changed.Members = slices.Clone(prepared.Members)
	pinCopy := *prepared.Members[0].EvidencePin
	pinCopy.SourceMetadataGenerationID = "70000000-0000-4000-8000-000000000002"
	changed.Members[0].EvidencePin = &pinCopy
	_, _, err = CanonicalPreparedProduction(changed)
	require.Error(t, err, "the approval subject must bind exact evidence pins")
}

func TestPreparedProductionRejectsMismatchedFactsPinAndContractVersions(t *testing.T) {
	base := syntheticPreparedProductionContract()
	_, factsSHA256, err := CanonicalPolicyMemberFacts(base.Members[0].Facts)
	require.NoError(t, err)
	validV2 := preparedProductionV2WithPin(t, syntheticPolicy(),
		redaction.FamilyContext{Kind: "standalone", RootVersionID: versionOneID},
		ProductionMemberEvidencePin{
			AllowlistVersion: PolicyFactsAllowlistV1, SourceMetadataGenerationID: "70000000-0000-4000-8000-000000000001",
			SourceMetadataEvidenceSHA256: sha("1"), PolicyFactsSHA256: factsSHA256,
		})
	_, _, err = CanonicalPreparedProduction(validV2)
	require.NoError(t, err, "the prepared v2 authority must be valid before changing its pin")

	t.Run("pin disagrees with member facts", func(t *testing.T) {
		changed := validV2
		changed.Members = slices.Clone(validV2.Members)
		pin := *validV2.Members[0].EvidencePin
		pin.PolicyFactsSHA256 = sha("f")
		require.NotEqual(t, factsSHA256, pin.PolicyFactsSHA256)
		changed.Members[0].EvidencePin = &pin
		_, changed.MemberHash, err = PreparedMemberHash(changed.Members)
		require.NoError(t, err)
		_, changed.ApprovalSubject.GateEvidenceSHA256, err = CanonicalProductionGateEvidence(changed.Members)
		require.NoError(t, err)
		require.NotEqual(t, validV2.ApprovalSubject.GateEvidenceSHA256, changed.ApprovalSubject.GateEvidenceSHA256)

		_, _, err = CanonicalPreparedProduction(changed)
		requireInvalidContractDetail(t, err, "prepared member facts do not match evidence pin")
	})

	t.Run("v1 cannot carry an evidence pin", func(t *testing.T) {
		_, _, err := CanonicalPreparedProduction(base)
		require.NoError(t, err)
		changed := base
		changed.Members = slices.Clone(base.Members)
		changed.Members[0].EvidencePin = &ProductionMemberEvidencePin{}
		_, _, err = CanonicalPreparedProduction(changed)
		requireInvalidContractDetail(t, err, "prepared member evidence pin does not match contract version")
	})

	t.Run("v2 requires an evidence pin", func(t *testing.T) {
		changed := validV2
		changed.Members = slices.Clone(validV2.Members)
		changed.Members[0].EvidencePin = nil
		_, _, err := CanonicalPreparedProduction(changed)
		requireInvalidContractDetail(t, err, "prepared member evidence pin does not match contract version")
	})
}

func TestPreparedProductionV2AllowsExplicitEmptyPinForGenericPolicy(t *testing.T) {
	prepared := preparedProductionV2WithPin(t, genericPolicyForGateTest(t),
		redaction.FamilyContext{Kind: "standalone", RootVersionID: versionOneID},
		ProductionMemberEvidencePin{})
	_, digest, err := CanonicalPreparedProduction(prepared)
	require.NoError(t, err, "the non-nil empty pin explicitly records no metadata/email evidence requirement")
	require.NotEmpty(t, digest)
}

func TestPreparedProductionV2RequiresPinnedMetadataFactsWhenConfigured(t *testing.T) {
	policy := syntheticPolicy()
	_, policy.SHA256, _ = CanonicalPolicyVersion(policy)
	prepared := preparedProductionV2WithPin(t, policy,
		redaction.FamilyContext{Kind: "standalone", RootVersionID: versionOneID},
		ProductionMemberEvidencePin{})
	_, _, err := CanonicalPreparedProduction(prepared)
	requireInvalidContractDetail(t, err, "configured metadata policy lacks pinned facts")
}

func TestPreparedProductionV2RequiresExactEmailPublicationFamilyPin(t *testing.T) {
	policy := genericPolicyForGateTest(t)
	family := redaction.FamilyContext{Kind: "email_message", RootVersionID: versionOneID}
	validPin := ProductionMemberEvidencePin{
		EmailRootVersionID: versionOneID, EmailPublicationOperationID: "publication_42",
		EmailPublicationRequestSHA256: sha("3"), EmailPublicationReceiptSHA256: sha("4"),
	}
	valid := preparedProductionV2WithPin(t, policy, family, validPin)
	_, _, err := CanonicalPreparedProduction(valid)
	require.NoError(t, err)
	t.Run("maximum length operation", func(t *testing.T) {
		pin := validPin
		pin.EmailPublicationOperationID = strings.Repeat("a", 128)
		prepared := preparedProductionV2WithPin(t, policy, family, pin)
		_, _, err := CanonicalPreparedProduction(prepared)
		require.NoError(t, err)
	})
	for _, operationID := range []string{"", "_leading", "with space", "with/slash", strings.Repeat("a", 129)} {
		t.Run("invalid operation "+operationID, func(t *testing.T) {
			pin := validPin
			pin.EmailPublicationOperationID = operationID
			member := valid.Members[0]
			member.EvidencePin = &pin
			_, _, err := CanonicalProductionGateEvidence([]PreparedMember{member})
			requireInvalidContractDetail(t, err, "invalid production email publication evidence pin")
		})
	}

	t.Run("missing publication", func(t *testing.T) {
		prepared := preparedProductionV2WithPin(t, policy, family, ProductionMemberEvidencePin{})
		_, _, err := CanonicalPreparedProduction(prepared)
		requireInvalidContractDetail(t, err, "email family member lacks publication evidence")
	})
	t.Run("wrong root", func(t *testing.T) {
		pin := validPin
		pin.EmailRootVersionID = versionTwoID
		prepared := preparedProductionV2WithPin(t, policy, family, pin)
		_, _, err := CanonicalPreparedProduction(prepared)
		requireInvalidContractDetail(t, err, "email family member lacks publication evidence")
	})
	t.Run("publication on non-email family", func(t *testing.T) {
		prepared := preparedProductionV2WithPin(t, policy,
			redaction.FamilyContext{Kind: "standalone", RootVersionID: versionOneID}, validPin)
		_, _, err := CanonicalPreparedProduction(prepared)
		requireInvalidContractDetail(t, err, "email publication evidence selects another family")
	})
}

func genericPolicyForGateTest(t *testing.T) PolicyVersion {
	t.Helper()
	policy, err := GenericPolicyVersion()
	require.NoError(t, err)
	return policy
}

func preparedProductionV2WithPin(
	t *testing.T, policy PolicyVersion, family redaction.FamilyContext, pin ProductionMemberEvidencePin,
) PreparedProduction {
	t.Helper()
	_, policySHA256, err := CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	policy.SHA256 = policySHA256
	prepared := syntheticPreparedProductionContract()
	prepared.Contract = PreparedProductionContractV2
	prepared.Policy = policy
	prepared.Members = slices.Clone(prepared.Members)
	prepared.Members[0].Member.Family = family
	prepared.Members[0].EvidencePin = &pin
	_, prepared.MemberHash, err = PreparedMemberHash(prepared.Members)
	require.NoError(t, err)
	_, evidenceSHA256, err := CanonicalProductionGateEvidence(prepared.Members)
	require.NoError(t, err)
	prepared.ApprovalSubject.Contract = ApprovalSubjectContractV2
	prepared.ApprovalSubject.GateEvidenceSHA256 = evidenceSHA256
	prepared.ApprovalSubject.Policy = PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256}
	return prepared
}

func TestPreparedInputAuthorityBindsReceiptAndAudit(t *testing.T) {
	prepared := syntheticPreparedProductionContract()
	_, prepared.SHA256, _ = CanonicalPreparedProduction(prepared)
	results := GateResults{Contract: GateResultsContractV1, PreparedSHA256: prepared.SHA256,
		EvaluatedAt: "2026-09-22T16:00:00Z", Checks: passingSyntheticGateChecks()}
	_, results.SHA256, _ = CanonicalGateResults(results)
	_, subjectSHA256, err := CanonicalApprovalSubject(prepared.ApprovalSubject)
	require.NoError(t, err)
	receipt := PreparedInputReceipt{Contract: PreparedInputContractV1,
		ID: "17171717-1717-4717-8717-171717171717", ApprovalSubjectSHA256: subjectSHA256,
		GateResultsSHA256: results.SHA256, CreatedAt: "2026-09-22T16:00:00Z"}
	_, receipt.SHA256, _ = CanonicalPreparedInputReceipt(receipt)
	audit := GateAudit{Contract: GateAuditContractV1,
		OperationID: "18181818-1818-4818-8818-181818181818", PreparedSHA256: prepared.SHA256,
		GateResultsSHA256: results.SHA256, PreparedInputSHA256: receipt.SHA256,
		RecordedAt: "2026-09-22T16:00:00Z"}
	_, audit.SHA256, _ = CanonicalGateAudit(audit)

	authority := PreparedInputAuthority{Prepared: prepared, GateResults: results, Receipt: &receipt, Audit: audit}
	require.NoError(t, ValidatePreparedInputAuthority(authority))

	authority.Receipt.GateResultsSHA256 = sha("f")
	require.Error(t, ValidatePreparedInputAuthority(authority), "a wrong receipt must not authorize numbering")
}

func TestPreparedInputAuthorityBindsConditionalAuthorities(t *testing.T) {
	authority := syntheticConditionalPreparedInputAuthority(t)
	require.NoError(t, ValidatePreparedInputAuthority(authority))

	t.Run("approval members", func(t *testing.T) {
		changed := authority.Prepared
		changed.SHA256 = ""
		changed.ApprovalSubject.Members = slices.Clone(changed.ApprovalSubject.Members)
		changed.ApprovalSubject.Members[0].SourceVersionID = versionTwoID
		_, _, err := CanonicalPreparedProduction(changed)
		require.Error(t, err)
	})

	t.Run("withheld selection", func(t *testing.T) {
		changed := authority.Prepared
		changed.SHA256 = ""
		selection := *changed.WithheldSelection
		selection.Members = slices.Clone(selection.Members)
		selection.Members[0].SourceSHA256 = sha("d")
		selection.SHA256 = ""
		_, selection.SHA256, _ = CanonicalWithheldSelection(selection)
		changed.WithheldSelection = &selection
		_, _, err := CanonicalPreparedProduction(changed)
		require.Error(t, err)
	})

	t.Run("privilege inputs", func(t *testing.T) {
		changed := authority.Prepared
		changed.SHA256 = ""
		receipt := *changed.PrivilegeLogReceipt
		receipt.InputsSHA256 = sha("e")
		receipt.SHA256 = ""
		_, receipt.SHA256, _ = CanonicalPrivilegeLogReceipt(receipt)
		changed.PrivilegeLogReceipt = &receipt
		_, _, err := CanonicalPreparedProduction(changed)
		require.Error(t, err)
	})

	t.Run("receipt conditional digests", func(t *testing.T) {
		changed := authority
		receipt := *changed.Receipt
		receipt.WithheldSelectionSHA256 = sha("d")
		receipt.SHA256 = ""
		_, receipt.SHA256, _ = CanonicalPreparedInputReceipt(receipt)
		changed.Receipt = &receipt
		changed.Audit.PreparedInputSHA256 = receipt.SHA256
		changed.Audit.SHA256 = ""
		_, changed.Audit.SHA256, _ = CanonicalGateAudit(changed.Audit)
		require.Error(t, ValidatePreparedInputAuthority(changed))
	})
}

func TestPreparedInputAuthorityPreservesFreezeTimeApprovalAndAdmitsLaterEvaluation(t *testing.T) {
	authority := syntheticConditionalPreparedInputAuthority(t)
	freezeEvaluation := *authority.Prepared.ApprovalEvaluation
	freezeEvaluation.EvaluatedAt = "2026-09-22T15:30:00Z"
	freezeEvaluation.SHA256 = ""
	var err error
	_, freezeEvaluation.SHA256, err = CanonicalApprovalEvaluation(freezeEvaluation)
	require.NoError(t, err)
	require.NotEqual(t, freezeEvaluation.SHA256, authority.Prepared.ApprovalEvaluation.SHA256,
		"an unchanged approval evaluated later has a distinct digest")

	privilege := *authority.Prepared.PrivilegeLogReceipt
	privilege.ApprovalEvaluationSHA256 = freezeEvaluation.SHA256
	privilege.SHA256 = ""
	_, privilege.SHA256, err = CanonicalPrivilegeLogReceipt(privilege)
	require.NoError(t, err)
	authority.Prepared.PrivilegeLogReceipt = &privilege
	resealPreparedInputAuthority(t, &authority)
	require.Equal(t, freezeEvaluation.SHA256, authority.Prepared.PrivilegeLogReceipt.ApprovalEvaluationSHA256)
	require.Equal(t, authority.Prepared.ApprovalEvaluation.SHA256, authority.Receipt.ApprovalEvaluationSHA256)
	require.NoError(t, ValidatePreparedInputAuthority(authority))
	_, subjectSHA256, err := CanonicalApprovalSubject(authority.Prepared.ApprovalSubject)
	require.NoError(t, err)
	require.NoError(t, ApprovalGateProblem(true, authority.Prepared.ApprovalEvaluation, subjectSHA256))
	require.NoError(t, PrivilegeLogGateProblem(true, authority.Prepared.PrivilegeLogReceipt,
		authority.Prepared.Policy.SHA256, authority.Prepared.WithheldSelection.SHA256,
		authority.Prepared.PrivilegeLogReceipt.PlayersSHA256, freezeEvaluation.SHA256,
		authority.Prepared.ApprovalSubject.PrivilegeLogInputsSHA256))

	for _, changed := range []struct {
		name string
		set  func(*PreparedInputAuthority)
	}{
		{"policy", func(value *PreparedInputAuthority) { value.Prepared.PrivilegeLogReceipt.PolicySHA256 = sha("d") }},
		{"withheld selection", func(value *PreparedInputAuthority) {
			value.Prepared.PrivilegeLogReceipt.WithheldSelectionSHA256 = sha("d")
		}},
	} {
		t.Run(changed.name, func(t *testing.T) {
			invalid := authority
			receipt := *authority.Prepared.PrivilegeLogReceipt
			invalid.Prepared.PrivilegeLogReceipt = &receipt
			changed.set(&invalid)
			receipt.SHA256 = ""
			_, receipt.SHA256, err = CanonicalPrivilegeLogReceipt(receipt)
			require.NoError(t, err)
			resealPreparedInputAuthority(t, &invalid)
			requireProblemCode(t, ValidatePreparedInputAuthority(invalid), ProblemChangedPayload)
		})
	}
	t.Run("inputs", func(t *testing.T) {
		invalid := authority
		receipt := *authority.Prepared.PrivilegeLogReceipt
		receipt.InputsSHA256 = sha("d")
		receipt.SHA256 = ""
		_, receipt.SHA256, err = CanonicalPrivilegeLogReceipt(receipt)
		require.NoError(t, err)
		invalid.Prepared.PrivilegeLogReceipt = &receipt
		requireProblemCode(t, ValidatePreparedInputAuthority(invalid), ProblemChangedPayload)
		requireProblemCode(t, PrivilegeLogGateProblem(true, &receipt,
			authority.Prepared.Policy.SHA256, authority.Prepared.WithheldSelection.SHA256,
			receipt.PlayersSHA256, freezeEvaluation.SHA256,
			authority.Prepared.ApprovalSubject.PrivilegeLogInputsSHA256), ProblemPrivilegeLogStale)
	})
}

func resealPreparedInputAuthority(t *testing.T, authority *PreparedInputAuthority) {
	t.Helper()
	var err error
	authority.Prepared.SHA256 = ""
	_, authority.Prepared.SHA256, err = CanonicalPreparedProduction(authority.Prepared)
	require.NoError(t, err)
	authority.GateResults.PreparedSHA256 = authority.Prepared.SHA256
	authority.GateResults.SHA256 = ""
	_, authority.GateResults.SHA256, err = CanonicalGateResults(authority.GateResults)
	require.NoError(t, err)
	authority.Receipt.PrivilegeLogReceiptSHA256 = authority.Prepared.PrivilegeLogReceipt.SHA256
	authority.Receipt.GateResultsSHA256 = authority.GateResults.SHA256
	authority.Receipt.SHA256 = ""
	_, authority.Receipt.SHA256, err = CanonicalPreparedInputReceipt(*authority.Receipt)
	require.NoError(t, err)
	authority.Audit.PreparedSHA256 = authority.Prepared.SHA256
	authority.Audit.GateResultsSHA256 = authority.GateResults.SHA256
	authority.Audit.PreparedInputSHA256 = authority.Receipt.SHA256
	authority.Audit.SHA256 = ""
	_, authority.Audit.SHA256, err = CanonicalGateAudit(authority.Audit)
	require.NoError(t, err)
}

func syntheticConditionalPreparedInputAuthority(t *testing.T) PreparedInputAuthority {
	t.Helper()
	prepared := syntheticPreparedProductionContract()
	member := prepared.Members[0]
	selection := WithheldSelection{
		Contract: WithheldSelectionContractV1, ID: "19191919-1919-4919-8919-191919191919",
		SetID: prepared.SetID, Revision: prepared.Revision, PolicySHA256: prepared.Policy.SHA256,
		Members: []WithheldMember{{ID: member.Member.ID, Ordinal: member.Member.Ordinal,
			SourceVersionID: member.Member.SourceVersionID, SourceSHA256: member.Member.SourceSHA256,
			SourceSize: member.Member.SourceSize, FamilyOrder: 1, Family: member.Member.Family}},
	}
	_, selectionSHA256, err := CanonicalWithheldSelection(selection)
	require.NoError(t, err)
	selection.SHA256 = selectionSHA256
	prepared.WithheldSelection = &selection
	prepared.ApprovalSubject.WithheldSelectionSHA256 = selection.SHA256
	prepared.ApprovalSubject.PrivilegeLogInputsSHA256 = sha("0")
	_, subjectSHA256, err := CanonicalApprovalSubject(prepared.ApprovalSubject)
	require.NoError(t, err)
	evaluation := ApprovalEvaluation{Contract: ApprovalEvaluationContractV1,
		ApprovalSHA256: sha("1"), EventsSHA256: sha("2"), SubjectSHA256: subjectSHA256,
		EvaluatedAt: "2026-09-22T16:00:00Z", State: ApprovalStateCurrent}
	_, evaluation.SHA256, err = CanonicalApprovalEvaluation(evaluation)
	require.NoError(t, err)
	privilege := PrivilegeLogReceipt{Contract: PrivilegeLogReceiptContractV1,
		LogID: "20202020-2020-4020-8020-202020202020", Revision: 1, State: PrivilegeLogStateFrozen,
		WithheldSelectionSHA256: selection.SHA256, PolicySHA256: prepared.Policy.SHA256,
		PlayersSHA256: sha("3"), ApprovalEvaluationSHA256: evaluation.SHA256,
		RowsSHA256: sha("4"), InputsSHA256: prepared.ApprovalSubject.PrivilegeLogInputsSHA256,
		RowCount: 1, ValidatedAt: "2026-09-22T15:00:00Z", FrozenAt: "2026-09-22T15:30:00Z"}
	_, privilege.SHA256, err = CanonicalPrivilegeLogReceipt(privilege)
	require.NoError(t, err)
	prepared.PrivilegeLogReceipt = &privilege
	prepared.ApprovalEvaluation = &evaluation
	_, prepared.SHA256, err = CanonicalPreparedProduction(prepared)
	require.NoError(t, err)
	results := GateResults{Contract: GateResultsContractV1, PreparedSHA256: prepared.SHA256,
		EvaluatedAt: "2026-09-22T16:00:00Z", Checks: passingSyntheticGateChecks()}
	_, results.SHA256, err = CanonicalGateResults(results)
	require.NoError(t, err)
	receipt := PreparedInputReceipt{Contract: PreparedInputContractV1,
		ID: "21212121-2121-4121-8121-212121212121", ApprovalSubjectSHA256: subjectSHA256,
		ApprovalEvaluationSHA256: evaluation.SHA256, WithheldSelectionSHA256: selection.SHA256,
		PrivilegeLogReceiptSHA256: privilege.SHA256, GateResultsSHA256: results.SHA256,
		CreatedAt: "2026-09-22T16:00:00Z"}
	_, receipt.SHA256, err = CanonicalPreparedInputReceipt(receipt)
	require.NoError(t, err)
	audit := GateAudit{Contract: GateAuditContractV1,
		OperationID: "22222222-2222-4222-8222-222222222222", PreparedSHA256: prepared.SHA256,
		GateResultsSHA256: results.SHA256, PreparedInputSHA256: receipt.SHA256,
		RecordedAt: "2026-09-22T16:00:00Z"}
	_, audit.SHA256, err = CanonicalGateAudit(audit)
	require.NoError(t, err)
	return PreparedInputAuthority{Prepared: prepared, GateResults: results, Receipt: &receipt, Audit: audit}
}

func syntheticPreparedProductionContract() PreparedProduction {
	policy, err := GenericPolicyVersion()
	if err != nil {
		panic(err)
	}
	m := redactiontest.Map("A")
	recipe, err := pdfproduction.QualifiedRecipeForDPI(300)
	if err != nil {
		panic(err)
	}
	resolved, err := redaction.Resolve(m, "redact_selected", nil, recipe)
	if err != nil {
		panic(err)
	}
	_, decisionsSHA256, err := redaction.CanonicalDecisions(nil)
	if err != nil {
		panic(err)
	}
	prepared := PreparedProduction{
		Contract: PreparedProductionContractV1,
		SetID:    setID, Revision: 7, ETag: 11, MembershipSealed: true,
		InstructionsSHA256: sha("1"), DecisionsSHA256: decisionsSHA256,
		RecipeSHA256: resolved.RecipeSHA256, OutputProfileSHA256: sha("5"), DisclosureProfileSHA256: sha("6"),
		NumberingPolicySHA256: sha("7"), Policy: policy, PreparedAt: "2026-09-22T16:00:00Z",
		Members: []PreparedMember{{
			Member: redaction.Member{ID: memberOneID, VaultID: setID, Ordinal: 1, SourceVersionID: versionOneID,
				SourceSHA256: sha("9"), SourceSize: 10, PDFSHA256: sha("a"), PDFSize: 20,
				MapSHA256: m.SHA256, PageInventorySHA256: sha("c"), Mode: "redact_selected",
				NodeID: 1, Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionOneID},
				Reviewed: true, ReviewBinding: sha("f")},
			Decisions: []redaction.Decision{}, Resolved: resolved,
			Frames:          []PreparedFrame{{Page: 1, SHA256: m.Pages[0].FrameSHA256, Width: m.Pages[0].Width, Height: m.Pages[0].Height}},
			Facts:           PolicyMemberFacts{MemberID: memberOneID, Fields: map[string][]string{}, Labels: []string{}},
			DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolved.SHA256, ReviewBinding: sha("f"),
			Disposition: PolicyDispositionProduce,
		}},
	}
	_, prepared.MemberHash, err = PreparedMemberHash(prepared.Members)
	if err != nil {
		panic(err)
	}
	prepared.ApprovalSubject = ApprovalSubject{
		Contract: ApprovalSubjectContractV1, SetID: prepared.SetID, Revision: prepared.Revision,
		Members: []ApprovalMember{{MemberID: memberOneID, Ordinal: 1, SourceVersionID: versionOneID,
			SourceSHA256: sha("9"), SourceSize: 10, PDFSHA256: sha("a"), PageInventorySHA256: sha("c"),
			MapSHA256: m.SHA256, DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolved.SHA256}},
		InstructionsSHA256: prepared.InstructionsSHA256, RecipeSHA256: prepared.RecipeSHA256,
		OutputProfileSHA256: prepared.OutputProfileSHA256, DisclosureProfileSHA256: prepared.DisclosureProfileSHA256,
		NumberingPolicySHA256: prepared.NumberingPolicySHA256,
		Policy:                PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256},
	}
	return prepared
}

func passingSyntheticGateChecks() []GateCheck {
	checks := make([]GateCheck, 0, len(requiredGateCheckIDs))
	for _, id := range requiredGateCheckIDs {
		checks = append(checks, GateCheck{ID: id, Required: true, State: GateStatePass,
			Evidence: []GateEvidence{{Kind: "digest", SHA256: sha("a")}}})
	}
	return checks
}
