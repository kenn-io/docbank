package production

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/redaction"
)

func TestPrivilegeFamilyOrderAndFields(t *testing.T) {
	policy, selection, players, rows := privilegeValidationFixture(t)
	input := PrivilegeLogValidationInput{
		LogID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Revision: 5,
		WithheldSelectionSHA256: selection.SHA256, PolicySHA256: policy.SHA256,
		PlayersSHA256: players.SHA256, ValidatedAt: "2026-09-22T03:00:00Z", Rows: rows,
	}

	validation, err := ValidatePrivilegeLog(input, selection, policy, players, nil)
	require.NoError(t, err)
	require.Equal(t, int64(5), validation.Inputs.Revision)
	require.NotEmpty(t, validation.RowsSHA256)
	require.NotEmpty(t, validation.InputsSHA256)

	tests := map[string]func(*PrivilegeLogValidationInput, *WithheldSelection){
		"family gap": func(inputValue *PrivilegeLogValidationInput, value *WithheldSelection) {
			value.Members[1].FamilyOrder = 3
			_, value.SHA256, _ = CanonicalWithheldSelection(withoutWithheldDigest(*value))
			inputValue.WithheldSelectionSHA256 = value.SHA256
		},
		"source version mismatch": func(value *PrivilegeLogValidationInput, _ *WithheldSelection) {
			value.Rows[1].SourceVersionID = "99999999-9999-4999-8999-999999999999"
		},
		"missing required field": func(value *PrivilegeLogValidationInput, _ *WithheldSelection) {
			value.Rows[0].Fields = nil
		},
		"basis outside vocabulary": func(value *PrivilegeLogValidationInput, _ *WithheldSelection) {
			value.Rows[0].Basis = "invented_basis"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changedInput := input
			changedInput.Rows = slices.Clone(input.Rows)
			for index := range changedInput.Rows {
				changedInput.Rows[index].Fields = slices.Clone(input.Rows[index].Fields)
			}
			changedSelection := selection
			changedSelection.Members = slices.Clone(selection.Members)
			mutate(&changedInput, &changedSelection)
			_, validationErr := ValidatePrivilegeLog(changedInput, changedSelection, policy, players, nil)
			requirePrivilegeValidationProblem(t, validationErr)
		})
	}

	produced := []redaction.Member{{
		ID: selection.Members[0].ID, SourceVersionID: selection.Members[0].SourceVersionID,
		Ordinal: selection.Members[0].Ordinal,
	}}
	_, err = ValidatePrivilegeLog(input, selection, policy, players, produced)
	requirePrivilegeValidationProblem(t, err)
}

func TestPrivilegeRowsUseDeterministicFamilyOrder(t *testing.T) {
	_, selection, _, rows := privilegeValidationFixture(t)
	ordered, err := OrderPrivilegeRows(selection, rows)
	require.NoError(t, err)
	require.Equal(t, selection.Members[1].ID, ordered[0].WithheldMemberID)
	require.Equal(t, selection.Members[0].ID, ordered[1].WithheldMemberID)

	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	reordered, err := OrderPrivilegeRows(selection, reversed)
	require.NoError(t, err)
	require.Equal(t, ordered, reordered)
}

func privilegeValidationFixture(t *testing.T) (PolicyVersion, WithheldSelection, PlayersSnapshot, []PrivilegeRow) {
	t.Helper()
	const (
		rootID     = "11111111-1111-4111-8111-111111111111"
		memberOne  = "22222222-2222-4222-8222-222222222222"
		memberTwo  = "33333333-3333-4333-8333-333333333333"
		versionOne = "44444444-4444-4444-8444-444444444444"
		versionTwo = "55555555-5555-4555-8555-555555555555"
	)
	policy := PolicyVersion{
		Contract: PolicyContractV1, ID: "66666666-6666-4666-8666-666666666666", Version: 4,
		Name: "Synthetic privilege policy", CreatedAt: "2026-09-22T01:00:00Z",
		Rules: []PolicyRule{{ID: "withhold", Kind: PolicyRuleDisposition,
			Predicate:   PolicyPredicate{Field: "member.id", Operator: PolicyOperatorPresent},
			Disposition: PolicyDispositionWithhold}},
		Approval: ApprovalRequirement{Required: true, EvidenceRequired: true, MaxAgeSeconds: 3600},
		PrivilegeLog: PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
			RequiredFields: []string{"date", "document_type"}, AllowedBases: []string{"synthetic_basis"}},
		ConflictMode: PolicyConflictReject,
	}
	_, policy.SHA256, _ = CanonicalPolicyVersion(policy)
	selection := WithheldSelection{
		Contract: WithheldSelectionContractV1, ID: "77777777-7777-4777-8777-777777777777",
		SetID: "88888888-8888-4888-8888-888888888888", Revision: 2, PolicySHA256: policy.SHA256,
		Members: []WithheldMember{
			{ID: memberTwo, Ordinal: 2, SourceVersionID: versionTwo, SourceSHA256: privilegeTestSHA("2"), SourceSize: 22,
				FamilyOrder: 2, Family: redaction.FamilyContext{Kind: "email_attachment", RootVersionID: rootID, RelationOperationID: "synthetic-relation", RelationOrder: 2}},
			{ID: memberOne, Ordinal: 1, SourceVersionID: versionOne, SourceSHA256: privilegeTestSHA("1"), SourceSize: 11,
				FamilyOrder: 1, Family: redaction.FamilyContext{Kind: "email_attachment", RootVersionID: rootID, RelationOperationID: "synthetic-relation", RelationOrder: 1}},
		},
	}
	_, selection.SHA256, _ = CanonicalWithheldSelection(selection)
	players := PlayersSnapshot{Contract: PlayersSnapshotContractV1, ID: "99999999-9999-4999-8999-999999999999", Revision: 3,
		Players: []Player{{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: privilegeTestSHA("3")}}}
	_, players.SHA256, _ = CanonicalPlayersSnapshot(players)
	rows := []PrivilegeRow{
		{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", WithheldMemberID: memberTwo, FamilyOrder: 2,
			SourceVersionID: versionTwo, Basis: "synthetic_basis", PublicDescription: "Synthetic attachment.",
			PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: privilegeTestSHA("4"),
			PersonIDs: []string{players.Players[0].ID}, Fields: []PrivilegeField{{Name: "date", Value: "2026-09-22"}, {Name: "document_type", Value: "Attachment"}}},
		{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", WithheldMemberID: memberOne, FamilyOrder: 1,
			SourceVersionID: versionOne, Basis: "synthetic_basis", PublicDescription: "Synthetic message.",
			PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: privilegeTestSHA("5"),
			PersonIDs: []string{players.Players[0].ID}, Fields: []PrivilegeField{{Name: "document_type", Value: "Message"}, {Name: "date", Value: "2026-09-22"}}},
	}
	return policy, selection, players, rows
}

func withoutWithheldDigest(value WithheldSelection) WithheldSelection {
	value.SHA256 = ""
	return value
}

func requirePrivilegeValidationProblem(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Contains(t, []ProblemCode{ProblemInvalidContract, ProblemPolicyUnsatisfied}, problem.Code)
}

func privilegeTestSHA(char string) string { return strings.Repeat(char, 64) }
