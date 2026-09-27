package production

import (
	"testing"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
)

func TestResolveCreatePolicyDefaultsAndPinsSelection(t *testing.T) {
	resolved, selection, err := ResolveCreatePolicy(redaction.CreateRequest{
		OperationID: "11111111-1111-4111-8111-111111111111",
		Name:        "Synthetic set",
	}, nil)
	require.NoError(t, err)
	require.False(t, resolved.Approval.Required)
	require.Equal(t, redaction.PolicySelection{
		PolicyID: resolved.ID, Version: resolved.Version, PolicySHA256: resolved.SHA256,
	}, selection)

	custom := internalPolicyTestVersion()
	_, digest, err := documentproduction.CanonicalPolicyVersion(custom)
	require.NoError(t, err)
	custom.SHA256 = digest
	request := redaction.CreateRequest{
		OperationID: "11111111-1111-4111-8111-111111111111", Name: "Synthetic set",
		PolicyID: custom.ID, PolicyVersion: custom.Version,
	}
	resolved, selection, err = ResolveCreatePolicy(request, func(id string, version int64) (documentproduction.PolicyVersion, error) {
		require.Equal(t, custom.ID, id)
		require.Equal(t, custom.Version, version)
		return custom, nil
	})
	require.NoError(t, err)
	require.Equal(t, custom.SHA256, resolved.SHA256)
	require.Equal(t, custom.SHA256, selection.PolicySHA256)
}

func TestPreparePolicyVersionCanonicalizesAndBindsRequest(t *testing.T) {
	policy := internalPolicyTestVersion()
	prepared, err := PreparePolicyVersion("11111111-1111-4111-8111-111111111111", policy)
	require.NoError(t, err)
	require.NotEmpty(t, prepared.Canonical)
	require.Equal(t, prepared.Policy.SHA256, prepared.PolicySHA256)
	require.NotEmpty(t, prepared.RequestSHA256)

	reordered := policy
	reordered.Rules = []documentproduction.PolicyRule{policy.Rules[1], policy.Rules[0]}
	retry, err := PreparePolicyVersion("11111111-1111-4111-8111-111111111111", reordered)
	require.NoError(t, err)
	require.Equal(t, prepared.RequestSHA256, retry.RequestSHA256)

	changed := policy
	changed.Output.ConfidentialityLabel = "SYNTHETIC CHANGED"
	changedPayload, err := PreparePolicyVersion("11111111-1111-4111-8111-111111111111", changed)
	require.NoError(t, err)
	require.NotEqual(t, prepared.RequestSHA256, changedPayload.RequestSHA256)
}

func internalPolicyTestVersion() documentproduction.PolicyVersion {
	return documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1, ID: "22222222-2222-4222-8222-222222222222", Version: 4,
		Name: "Synthetic policy", CreatedAt: "2026-09-22T00:00:00Z", ConflictMode: documentproduction.PolicyConflictReject,
		Rules: []documentproduction.PolicyRule{
			{ID: "scope", Kind: documentproduction.PolicyRuleScope, Predicate: documentproduction.PolicyPredicate{Field: "document.scope", Operator: documentproduction.PolicyOperatorEquals, Values: []string{"selected"}}, Disposition: documentproduction.PolicyDispositionProduce},
			{ID: "label", Kind: documentproduction.PolicyRuleLabel, Predicate: documentproduction.PolicyPredicate{Field: "document.scope", Operator: documentproduction.PolicyOperatorEquals, Values: []string{"selected"}}, RequiredLabel: "reviewed"},
		},
		Output:   documentproduction.PolicyOutput{ConfidentialityLabel: "SYNTHETIC", EndorsementKind: "legend", EndorsementText: "Synthetic output"},
		Approval: documentproduction.ApprovalRequirement{Required: true, EvidenceRequired: true, MaxAgeSeconds: 3600},
	}
}
