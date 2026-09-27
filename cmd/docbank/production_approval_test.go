package main

import (
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionApprovalCLIReadsPublicProjectionFromRealDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	policy, err := documentproduction.GenericPolicyVersion()
	require.NoError(t, err)
	policy.ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	policy.Name = "Synthetic approval policy"
	policy.CreatedAt = "2026-09-22T13:00:00Z"
	policy.SHA256 = ""
	preparedPolicy, err := productionservice.PreparePolicyVersion(
		"99999999-9999-4999-8999-999999999999", policy)
	require.NoError(t, err)
	policy, err = catalog.PutProductionPolicy(t.Context(), preparedPolicy)
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
	record, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "55555555-5555-4555-8555-555555555555", ApprovalID: approvalID,
		Subject: subject, Evidence: "Synthetic private evidence",
	}, policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: "2026-09-22T13:59:00Z", EvidenceSHA256: sha("c"),
		},
	}, time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	_, err = catalog.PutProductionApproval(t.Context(), record)
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)

	out, err := runCLI(t, "production", "approval", "show", approvalID, "--json")
	require.NoError(t, err)
	require.NotContains(t, out, "Synthetic private evidence")
	require.NotContains(t, out, "synthetic-principal")
	var public api.ProductionApprovalPublic
	require.NoError(t, json.Unmarshal([]byte(out), &public))
	require.Equal(t, approvalID, public.Grant.ID)
	require.Equal(t, "synthetic-reviewer", public.Grant.Actor)
	_, err = runCLI(t, "production", "approval", "show", "88888888-8888-4888-8888-888888888888")
	require.Error(t, err)
}
