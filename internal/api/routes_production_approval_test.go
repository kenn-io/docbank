package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/daemonconn"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestProductionApprovalReadExposesOnlyPublicProjection(t *testing.T) {
	ts, s := newTestServer(t, nil)
	policy, err := documentproduction.GenericPolicyVersion()
	require.NoError(t, err)
	policy.ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	policy.Name = "Synthetic approval policy"
	policy.CreatedAt = "2026-09-22T13:00:00Z"
	policy.SHA256 = ""
	preparedPolicy, err := productionservice.PreparePolicyVersion(
		"99999999-9999-4999-8999-999999999999", policy)
	require.NoError(t, err)
	policy, err = s.PutProductionPolicy(t.Context(), preparedPolicy)
	require.NoError(t, err)
	const approvalID = "11111111-1111-4111-8111-111111111111"
	privateEvidence := "Synthetic private approval evidence"
	privateReason := "Synthetic private revocation reason"
	sha := func(char string) string { return strings.Repeat(char, 64) }
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
		Subject: subject, Evidence: privateEvidence,
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
	grant, err := s.PutProductionApproval(t.Context(), record)
	require.NoError(t, err)
	event, err := productionservice.PrepareApprovalEvent(grant, productionservice.ApprovalEventRequest{
		OperationID: "66666666-6666-4666-8666-666666666666",
		EventID:     "77777777-7777-4777-8777-777777777777",
		Kind:        documentproduction.ApprovalEventRevoke,
		EffectiveAt: "2026-09-22T14:01:00Z", Reason: privateReason,
	})
	require.NoError(t, err)
	_, err = s.PutProductionApprovalEvent(t.Context(), event)
	require.NoError(t, err)

	response, body := get(t, ts, "/api/v1/production-approvals/"+approvalID, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NotContains(t, body, privateEvidence)
	require.NotContains(t, body, privateReason)
	require.NotContains(t, body, "synthetic-principal")
	var public struct {
		Grant  documentproduction.ApprovalPublicGrant   `json:"grant"`
		Events []documentproduction.ApprovalPublicEvent `json:"events"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &public))
	require.Equal(t, approvalID, public.Grant.ID)
	require.Equal(t, grant.SHA256, public.Grant.SHA256)
	require.Len(t, public.Events, 1)
	require.Equal(t, documentproduction.ApprovalEventRevoke, public.Events[0].Kind)
	client := daemonconn.New(ts.URL, testAPIKey)
	clientPublic, err := client.ProductionApproval(t.Context(), approvalID)
	require.NoError(t, err)
	require.Equal(t, struct {
		Grant  documentproduction.ApprovalPublicGrant   `json:"grant"`
		Events []documentproduction.ApprovalPublicEvent `json:"events"`
	}{Grant: clientPublic.Grant, Events: clientPublic.Events}, public)

	response, body = get(t, ts, "/api/v1/production-approvals/88888888-8888-4888-8888-888888888888", nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}
