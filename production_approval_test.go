package docbank_test

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	documentproduction "go.kenn.io/docbank/document/production"
)

func TestEmbeddedApprovalUsesHostAuthenticationAndReturnsPublicGrant(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	policy, err := documentproduction.GenericPolicyVersion()
	require.NoError(t, err)
	policy.ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	policy.Name = "Synthetic approval policy"
	policy.CreatedAt = "2026-09-22T13:00:00Z"
	policy.SHA256 = ""
	policy, err = vault.CreateProductionPolicyVersion(t.Context(),
		"99999999-9999-4999-8999-999999999999", policy)
	require.NoError(t, err)
	sha := func(char string) string { return strings.Repeat(char, 64) }
	const approvalID = "11111111-1111-4111-8111-111111111111"
	subject := documentproduction.ApprovalSubject{
		Contract: documentproduction.ApprovalSubjectContractV1,
		SetID:    "22222222-2222-4222-8222-222222222222", Revision: 1,
		Members: []documentproduction.ApprovalMember{{
			MemberID: "33333333-3333-4333-8333-333333333333", Ordinal: 1,
			SourceVersionID: "44444444-4444-4444-8444-444444444444",
			SourceSHA256:    sha("1"), SourceSize: 10, PDFSHA256: sha("2"),
			PageInventorySHA256: sha("3"), MapSHA256: sha("4"),
			DecisionsSHA256: sha("5"), ResolvedSHA256: sha("6"),
		}},
		InstructionsSHA256: sha("7"), RecipeSHA256: sha("8"),
		OutputProfileSHA256: sha("9"), DisclosureProfileSHA256: sha("a"),
		NumberingPolicySHA256: sha("b"),
		Policy: documentproduction.PolicySelection{PolicyID: policy.ID,
			Version: policy.Version, PolicySHA256: policy.SHA256},
	}
	request := docbank.ProductionApprovalRequest{
		OperationID: "55555555-5555-4555-8555-555555555555", ApprovalID: approvalID,
		Subject: subject, Evidence: "Synthetic private evidence",
	}
	auth := docbank.ProductionApprovalAuthentication{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: "2026-09-22T13:59:00Z", EvidenceSHA256: sha("c"),
		},
	}
	created, err := vault.RecordProductionApproval(t.Context(), request, auth)
	require.NoError(t, err)
	require.Equal(t, approvalID, created.ID)
	replay, err := vault.RecordProductionApproval(t.Context(), request, auth)
	require.NoError(t, err)
	require.Equal(t, created, replay)
	public, err := vault.ProductionApproval(t.Context(), approvalID)
	require.NoError(t, err)
	require.Equal(t, created, public.Grant)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), request.Evidence)
	require.NotContains(t, string(encoded), "synthetic-principal")

	request.Evidence = "Changed synthetic evidence"
	_, err = vault.RecordProductionApproval(t.Context(), request, auth)
	require.Error(t, err)
	_, err = vault.RecordProductionApproval(t.Context(), request, docbank.ProductionApprovalAuthentication{})
	require.Error(t, err)
}
