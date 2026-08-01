package client

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestValidateAuditVerifyBlobProblemsCountsAffectedBlobs(t *testing.T) {
	firstHash := "1111111111111111111111111111111111111111111111111111111111111111"
	secondHash := "2222222222222222222222222222222222222222222222222222222222222222"
	firstStore := "10000000-0000-4000-8000-000000000001"
	secondStore := "20000000-0000-4000-8000-000000000002"
	report := api.AuditVerifyReport{
		ProtectedBlobs: 2,
		Problems: []api.VerifyProblem{
			{Hash: firstHash, StoreID: firstStore, Problem: "missing"},
			{Hash: firstHash, StoreID: secondStore, Problem: "corrupt"},
			{Hash: secondHash, StoreID: firstStore, Problem: "unreadable"},
		},
	}

	require.NoError(t, validateAuditVerifyBlobProblems(report))

	report.Problems = append(report.Problems, report.Problems[len(report.Problems)-1])
	require.Error(t, validateAuditVerifyBlobProblems(report))
}

func TestValidateAuditVerifyBlobProblemsRejectsContradictoryStorelessEvidence(
	t *testing.T,
) {
	hash := "1111111111111111111111111111111111111111111111111111111111111111"
	storeID := "10000000-0000-4000-8000-000000000001"
	report := api.AuditVerifyReport{
		ProtectedBlobs: 1,
		Problems: []api.VerifyProblem{{
			Hash: hash, Problem: "corrupt",
		}},
	}
	require.Error(t, validateAuditVerifyBlobProblems(report))

	report.Problems = []api.VerifyProblem{
		{Hash: hash, Problem: "missing"},
		{Hash: hash, StoreID: storeID, Problem: "corrupt"},
	}
	require.Error(t, validateAuditVerifyBlobProblems(report))
}
