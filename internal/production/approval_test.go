package production

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
)

func TestPolicyChangeInvalidatesApproval(t *testing.T) {
	policy := approvalTestPolicy(t)
	subject := approvalTestSubject(policy)
	record := approvalTestRecord(t, policy, subject)

	changed := policy
	changed.Output.ConfidentialityLabel = "SYNTHETIC CHANGED"
	_, changedDigest, err := documentproduction.CanonicalPolicyVersion(changed)
	require.NoError(t, err)
	changed.SHA256 = changedDigest
	changedSubject := subject
	changedSubject.Policy.PolicySHA256 = changedDigest

	evaluation, err := EvaluateRequiredApproval(changed, changedSubject, &record.Grant, nil, mustApprovalTime(t, "2026-09-22T01:30:00Z"))
	require.Error(t, err)
	require.Equal(t, documentproduction.ApprovalStateStaleSubject, evaluation.State)
	requireApprovalProblem(t, err, documentproduction.ProblemApprovalStale)
}

func TestApprovalRejectsSameCountSourceSubstitution(t *testing.T) {
	policy := approvalTestPolicy(t)
	subject := approvalTestSubject(policy)
	record := approvalTestRecord(t, policy, subject)

	changed := subject
	changed.Members = append([]documentproduction.ApprovalMember(nil), subject.Members...)
	changed.Members[0].SourceVersionID = "77777777-7777-4777-8777-777777777777"
	changed.Members[0].SourceSHA256 = approvalTestSHA("f")
	changed.Members[0].SourceSize++

	evaluation, err := EvaluateRequiredApproval(policy, changed, &record.Grant, nil, mustApprovalTime(t, "2026-09-22T01:30:00Z"))
	require.Error(t, err)
	require.Equal(t, documentproduction.ApprovalStateStaleSubject, evaluation.State)
	requireApprovalProblem(t, err, documentproduction.ProblemApprovalStale)
}

func TestRequiredApprovalUsesAuthenticatedAuthority(t *testing.T) {
	policy := approvalTestPolicy(t)
	subject := approvalTestSubject(policy)
	request := RecordApprovalRequest{
		OperationID: "11111111-1111-4111-8111-111111111111",
		ApprovalID:  "33333333-3333-4333-8333-333333333333",
		Subject:     subject,
		Evidence:    "Synthetic private evidence.",
	}
	_, err := PrepareApprovalRecord(request, policy, AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract: documentproduction.ApprovalAuthorityContractV1,
			Kind:     documentproduction.ApprovalAuthorityAuthenticatedHuman, PrincipalID: "synthetic-principal",
			AuthenticationMethod: "synthetic-authentication", AuthenticatedAt: "2026-09-22T00:59:00Z",
			EvidenceSHA256: approvalTestSHA("a"),
		},
	}, mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.NoError(t, err)

	_, err = PrepareApprovalRecord(request, policy, AuthenticatedApproval{Actor: "synthetic-reviewer"}, mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.Error(t, err, "an agent-supplied actor name is provenance, not approval proof")

	request.Evidence = ""
	_, err = PrepareApprovalRecord(request, policy, approvalTestAuthority(), mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.Error(t, err, "policy-required evidence cannot be omitted")
}

func TestApprovalLifecycleAndWrongApprovalCannotPass(t *testing.T) {
	policy := approvalTestPolicy(t)
	subject := approvalTestSubject(policy)
	record := approvalTestRecord(t, policy, subject)

	_, err := EvaluateRequiredApproval(policy, subject, nil, nil, mustApprovalTime(t, "2026-09-22T01:30:00Z"))
	requireApprovalProblem(t, err, documentproduction.ProblemApprovalRequired)

	revocation, err := PrepareApprovalEvent(record.Grant, ApprovalEventRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", EventID: "99999999-9999-4999-8999-999999999999",
		Kind: documentproduction.ApprovalEventRevoke, EffectiveAt: "2026-09-22T01:15:00Z", Reason: "Synthetic private reason.",
	})
	require.NoError(t, err)
	_, err = EvaluateRequiredApproval(policy, subject, &record.Grant, []documentproduction.ApprovalEvent{revocation.Event}, mustApprovalTime(t, "2026-09-22T01:30:00Z"))
	requireApprovalProblem(t, err, documentproduction.ProblemApprovalStale)

	tampered := record.Grant
	tampered.SubjectSHA256 = approvalTestSHA("f")
	_, err = EvaluateRequiredApproval(policy, subject, &tampered, nil, mustApprovalTime(t, "2026-09-22T01:30:00Z"))
	require.Error(t, err)
}

func TestApprovalRequestDigestChangesWithPrivateEvidenceOrSubject(t *testing.T) {
	policy := approvalTestPolicy(t)
	subject := approvalTestSubject(policy)
	request := RecordApprovalRequest{OperationID: "11111111-1111-4111-8111-111111111111",
		ApprovalID: "33333333-3333-4333-8333-333333333333", Subject: subject, Evidence: "Synthetic private evidence."}
	first, err := PrepareApprovalRecord(request, policy, approvalTestAuthority(), mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.NoError(t, err)
	retry, err := PrepareApprovalRecord(request, policy, approvalTestAuthority(), mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, first.RequestSHA256, retry.RequestSHA256)
	require.Equal(t, first.Grant.SHA256, retry.Grant.SHA256)

	request.Evidence = "Changed synthetic private evidence."
	changed, err := PrepareApprovalRecord(request, policy, approvalTestAuthority(), mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.NoError(t, err)
	require.NotEqual(t, first.RequestSHA256, changed.RequestSHA256)

	request.Evidence = "Synthetic private evidence."
	request.Subject.Members = append([]documentproduction.ApprovalMember(nil), subject.Members...)
	request.Subject.Members[0].DecisionsSHA256 = approvalTestSHA("e")
	changedSubject, err := PrepareApprovalRecord(request, policy, approvalTestAuthority(), mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.NoError(t, err)
	require.NotEqual(t, first.RequestSHA256, changedSubject.RequestSHA256)
}

func approvalTestRecord(t *testing.T, policy documentproduction.PolicyVersion, subject documentproduction.ApprovalSubject) ApprovalRecord {
	t.Helper()
	record, err := PrepareApprovalRecord(RecordApprovalRequest{
		OperationID: "11111111-1111-4111-8111-111111111111",
		ApprovalID:  "33333333-3333-4333-8333-333333333333", Subject: subject, Evidence: "Synthetic private evidence.",
	}, policy, approvalTestAuthority(), mustApprovalTime(t, "2026-09-22T01:00:00Z"))
	require.NoError(t, err)
	return record
}

func approvalTestPolicy(t *testing.T) documentproduction.PolicyVersion {
	t.Helper()
	policy := internalPolicyTestVersion()
	_, digest, err := documentproduction.CanonicalPolicyVersion(policy)
	require.NoError(t, err)
	policy.SHA256 = digest
	return policy
}

func approvalTestSubject(policy documentproduction.PolicyVersion) documentproduction.ApprovalSubject {
	return documentproduction.ApprovalSubject{
		Contract: documentproduction.ApprovalSubjectContractV1,
		SetID:    "12121212-1212-4212-8212-121212121212", Revision: 7,
		Members: []documentproduction.ApprovalMember{{
			MemberID: "44444444-4444-4444-8444-444444444444", Ordinal: 1,
			SourceVersionID: "66666666-6666-4666-8666-666666666666", SourceSHA256: approvalTestSHA("1"), SourceSize: 11,
			PDFSHA256: approvalTestSHA("2"), PageInventorySHA256: approvalTestSHA("3"), MapSHA256: approvalTestSHA("4"),
			DecisionsSHA256: approvalTestSHA("5"), ResolvedSHA256: approvalTestSHA("6"),
		}},
		InstructionsSHA256: approvalTestSHA("7"), RecipeSHA256: approvalTestSHA("8"),
		OutputProfileSHA256: approvalTestSHA("9"), DisclosureProfileSHA256: approvalTestSHA("a"),
		NumberingPolicySHA256: approvalTestSHA("b"),
		Policy:                documentproduction.PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256},
	}
}

func approvalTestAuthority() AuthenticatedApproval {
	return AuthenticatedApproval{Actor: "synthetic-reviewer", Authority: documentproduction.ApprovalAuthority{
		Contract: documentproduction.ApprovalAuthorityContractV1,
		Kind:     documentproduction.ApprovalAuthorityAuthenticatedHuman, PrincipalID: "synthetic-principal",
		AuthenticationMethod: "synthetic-authentication", AuthenticatedAt: "2026-09-22T00:59:00Z",
		EvidenceSHA256: approvalTestSHA("c"),
	}}
}

func mustApprovalTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

func requireApprovalProblem(t *testing.T, err error, code documentproduction.ProblemCode) {
	t.Helper()
	require.Error(t, err)
	problem := &documentproduction.Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, code, problem.Code)
}

func approvalTestSHA(char string) string {
	result := ""
	var resultSb186 strings.Builder
	for range 64 {
		resultSb186.WriteString(char)
	}
	result += resultSb186.String()
	return result
}
