package production

import (
	"slices"
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
