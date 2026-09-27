package production

import (
	"encoding/json/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyConflictIsBoundedAndOrderIndependent(t *testing.T) {
	policy := policyTestVersion()
	policy.Rules = []PolicyRule{
		{ID: "withhold-rule", Kind: PolicyRuleDisposition, Predicate: PolicyPredicate{Field: "document.scope", Operator: PolicyOperatorEquals, Values: []string{"selected"}}, Disposition: PolicyDispositionWithhold},
		{ID: "produce-rule", Kind: PolicyRuleScope, Predicate: PolicyPredicate{Field: "document.scope", Operator: PolicyOperatorEquals, Values: []string{"selected"}}, Disposition: PolicyDispositionProduce},
	}

	_, err := EvaluatePolicy(policy, []PolicyMemberFacts{{
		MemberID: "44444444-4444-4444-8444-444444444444",
		Fields:   map[string][]string{"document.scope": {"selected"}},
	}})
	require.Error(t, err)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, ProblemPolicyUnsatisfied, problem.Code)
	require.Equal(t, []string{"produce-rule", "withhold-rule"}, problem.IDs)
	require.LessOrEqual(t, len(problem.IDs), MaxProblemIDs)

	slices.Reverse(policy.Rules)
	_, reorderedErr := EvaluatePolicy(policy, []PolicyMemberFacts{{
		MemberID: "44444444-4444-4444-8444-444444444444",
		Fields:   map[string][]string{"document.scope": {"selected"}},
	}})
	reordered := &Problem{}
	require.ErrorAs(t, reorderedErr, &reordered)
	require.Equal(t, problem.IDs, reordered.IDs)
}

func TestPolicyViolationSetDeduplicatesAndBounds(t *testing.T) {
	violations := newPolicyViolationSet()
	for range MaxPrivilegeRows {
		require.NoError(t, violations.add("repeated-rule"))
	}
	require.Len(t, violations, 1)

	for index := 1; index < MaxProblemIDs; index++ {
		require.NoError(t, violations.add("rule-"+strconv.Itoa(index)))
	}
	require.Len(t, violations, MaxProblemIDs)

	err := violations.add("overflow-rule")
	require.Error(t, err)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, ProblemLimit, problem.Code)
}

func TestPolicyRepeatedViolationsRemainDistinct(t *testing.T) {
	policy := policyTestVersion()
	policy.Rules = []PolicyRule{{
		ID: "label", Kind: PolicyRuleLabel,
		Predicate:     PolicyPredicate{Field: "member.id", Operator: PolicyOperatorPresent},
		RequiredLabel: "reviewed",
	}}
	members := make([]PolicyMemberFacts, 10_000)
	for index := range members {
		members[index] = PolicyMemberFacts{MemberID: "member-" + strconv.Itoa(index)}
	}

	_, err := EvaluatePolicy(policy, members)
	require.Error(t, err)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, ProblemPolicyUnsatisfied, problem.Code)
	require.Equal(t, []string{"label"}, problem.IDs)
}

func TestPolicyEvaluatesDateFamilyLabelsAndOutputRequirements(t *testing.T) {
	policy := policyTestVersion()
	policy.Rules = []PolicyRule{
		{ID: "date", Kind: PolicyRuleDate, Predicate: PolicyPredicate{Field: "document.date", Operator: PolicyOperatorDateBetween, From: "2026-01-01", Through: "2026-12-31"}, Disposition: PolicyDispositionProduce},
		{ID: "family", Kind: PolicyRuleFamily, Predicate: PolicyPredicate{Field: "family.kind", Operator: PolicyOperatorEquals, Values: []string{"email"}}, Disposition: PolicyDispositionProduce, FamilyMode: PolicyFamilyComplete},
		{ID: "label", Kind: PolicyRuleLabel, Predicate: PolicyPredicate{Field: "document.scope", Operator: PolicyOperatorEquals, Values: []string{"selected"}}, RequiredLabel: "reviewed"},
	}
	policy.Output = PolicyOutput{ConfidentialityLabel: "SYNTHETIC", EndorsementKind: "legend", EndorsementText: "Synthetic output"}
	policy.Approval = ApprovalRequirement{Required: true, EvidenceRequired: true, MaxAgeSeconds: 3600}

	members := []PolicyMemberFacts{
		{MemberID: "44444444-4444-4444-8444-444444444444", FamilyID: "family-a", FamilyComplete: true,
			Fields: map[string][]string{"document.date": {"2026-06-01"}, "document.scope": {"selected"}, "family.kind": {"email"}}, Labels: []string{"reviewed"}},
		{MemberID: "55555555-5555-4555-8555-555555555555", FamilyID: "family-a", FamilyComplete: true,
			Fields: map[string][]string{"document.date": {"2025-06-01"}, "document.scope": {"other"}, "family.kind": {"attachment"}}},
	}
	evaluation, err := EvaluatePolicy(policy, members)
	require.NoError(t, err)
	require.Equal(t, PolicyDispositionProduce, evaluation.Members[0].Disposition)
	require.Equal(t, PolicyDispositionProduce, evaluation.Members[1].Disposition,
		"a complete-family rule applies its disposition to every selected family occurrence")
	require.Equal(t, policy.Output, evaluation.Output)
	require.Equal(t, policy.Approval, evaluation.Approval)

	members[0].Labels = nil
	_, err = EvaluatePolicy(policy, members)
	require.Error(t, err)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, []string{"label"}, problem.IDs)

	members[0].Labels = []string{"reviewed"}
	members[0].FamilyComplete = false
	_, err = EvaluatePolicy(policy, members)
	require.ErrorAs(t, err, &problem)
	require.Equal(t, []string{"family"}, problem.IDs)
}

func TestPolicyRejectsMalformedDateFact(t *testing.T) {
	policy := policyTestVersion()
	policy.Rules = []PolicyRule{{
		ID: "date", Kind: PolicyRuleDate,
		Predicate:   PolicyPredicate{Field: "document.date", Operator: PolicyOperatorDateBetween, From: "2026-01-01", Through: "2026-12-31"},
		Disposition: PolicyDispositionProduce,
	}}

	_, err := EvaluatePolicy(policy, []PolicyMemberFacts{{
		MemberID: "44444444-4444-4444-8444-444444444444",
		Fields:   map[string][]string{"document.date": {"2026-06-00"}},
	}})
	require.Error(t, err)
	problem := &Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, ProblemInvalidContract, problem.Code)
}

func TestPolicyFamilyPredicateUsesStructuredFamilyID(t *testing.T) {
	policy := policyTestVersion()
	policy.Rules = []PolicyRule{{
		ID: "family", Kind: PolicyRuleFamily,
		Predicate:   PolicyPredicate{Field: "family.id", Operator: PolicyOperatorEquals, Values: []string{"family-a"}},
		Disposition: PolicyDispositionProduce,
		FamilyMode:  PolicyFamilyMember,
	}}

	evaluation, err := EvaluatePolicy(policy, []PolicyMemberFacts{{
		MemberID: "44444444-4444-4444-8444-444444444444",
		Fields:   map[string][]string{"family.id": {"family-a"}},
	}})
	require.NoError(t, err)
	require.Empty(t, evaluation.Members[0].Disposition)
	require.Empty(t, evaluation.Members[0].MatchedRuleIDs)
}

func TestPolicyCompleteFamilyScalesAcrossLargeFamily(t *testing.T) {
	policy := policyTestVersion()
	policy.Rules = []PolicyRule{{
		ID: "family", Kind: PolicyRuleFamily,
		Predicate:   PolicyPredicate{Field: "family.kind", Operator: PolicyOperatorEquals, Values: []string{"email"}},
		Disposition: PolicyDispositionProduce,
		FamilyMode:  PolicyFamilyComplete,
	}}
	members := make([]PolicyMemberFacts, 4096)
	for index := range members {
		members[index] = PolicyMemberFacts{
			MemberID:       "member-" + strconv.Itoa(index),
			FamilyID:       "family-a",
			FamilyComplete: true,
			Fields:         map[string][]string{"family.kind": {"email"}},
		}
	}

	evaluation, err := EvaluatePolicy(policy, members)
	require.NoError(t, err)
	for _, index := range []int{0, len(members) / 2, len(members) - 1} {
		require.Equal(t, PolicyDispositionProduce, evaluation.Members[index].Disposition)
		require.Equal(t, []string{"family"}, evaluation.Members[index].MatchedRuleIDs)
	}
}

func TestAutonomousDefaultNeedsNoApproval(t *testing.T) {
	policy, err := GenericPolicyVersion()
	require.NoError(t, err)
	require.False(t, policy.Approval.Required)
	require.NotEmpty(t, policy.SHA256)

	evaluation, err := EvaluatePolicy(policy, []PolicyMemberFacts{{
		MemberID: "44444444-4444-4444-8444-444444444444",
		Fields:   map[string][]string{"member.id": {"44444444-4444-4444-8444-444444444444"}},
	}})
	require.NoError(t, err)
	require.Equal(t, PolicyDispositionProduce, evaluation.Members[0].Disposition)
	require.NoError(t, ApprovalGateProblem(policy.Approval.Required, nil, policy.SHA256))
}

func TestApprovalPublicProjectionOmitsPrivateEvidenceAndReasons(t *testing.T) {
	grant := ApprovalGrant{ID: "33333333-3333-4333-8333-333333333333", SubjectSHA256: policyTestSHA("1"),
		Actor: "synthetic-reviewer", GrantedAt: "2026-09-22T01:00:00Z", ExpiresAt: "2026-09-22T02:00:00Z",
		Evidence: "private approval evidence", SHA256: policyTestSHA("2")}
	events := []ApprovalEvent{{ID: "88888888-8888-4888-8888-888888888888", ApprovalID: grant.ID,
		Kind: ApprovalEventRevoke, EffectiveAt: "2026-09-22T01:30:00Z", Reason: "private revocation reason"}}

	encoded, err := json.Marshal(struct {
		Grant  ApprovalPublicGrant   `json:"grant"`
		Events []ApprovalPublicEvent `json:"events"`
	}{PublicApprovalGrant(grant), PublicApprovalEvents(events)})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), grant.Evidence)
	require.NotContains(t, string(encoded), events[0].Reason)
	require.Contains(t, string(encoded), grant.Actor)
}

func policyTestVersion() PolicyVersion {
	return PolicyVersion{Contract: PolicyContractV1, ID: "22222222-2222-4222-8222-222222222222", Version: 1,
		Name: "Synthetic policy", CreatedAt: "2026-09-22T00:00:00Z", ConflictMode: PolicyConflictReject,
		Rules: []PolicyRule{{ID: "produce", Kind: PolicyRuleDisposition,
			Predicate: PolicyPredicate{Field: "member.id", Operator: PolicyOperatorPresent}, Disposition: PolicyDispositionProduce}}}
}

func policyTestSHA(char string) string {
	result := ""
	var resultSb119 strings.Builder
	for range 64 {
		resultSb119.WriteString(char)
	}
	result += resultSb119.String()
	return result
}
