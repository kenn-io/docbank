package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionPolicyCLIUsesDaemonForCreateListAndShow(t *testing.T) {
	_ = setupVaultHome(t)
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Version: 1,
		Name: "Synthetic CLI policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}}, ConflictMode: documentproduction.PolicyConflictReject,
	}
	encoded, err := json.Marshal(policy)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "synthetic-policy.json")
	require.NoError(t, os.WriteFile(file, encoded, 0o600))
	const operationID = "22222222-2222-4222-8222-222222222222"
	created, err := runCLI(t, "production", "policy", "create", "--file", file,
		"--operation-id", operationID, "--json")
	require.NoError(t, err)
	var stored documentproduction.PolicyVersion
	require.NoError(t, json.Unmarshal([]byte(created), &stored))
	require.NoError(t, documentproduction.ValidatePolicyVersion(stored))
	replay, err := runCLI(t, "production", "policy", "create", "--file", file,
		"--operation-id", operationID, "--json")
	require.NoError(t, err)
	var replayed documentproduction.PolicyVersion
	require.NoError(t, json.Unmarshal([]byte(replay), &replayed))
	require.Equal(t, stored.SHA256, replayed.SHA256)
	policy.Version = 2
	encoded, err = json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, encoded, 0o600))
	_, err = runCLI(t, "production", "policy", "create", "--file", file,
		"--operation-id", "33333333-3333-4333-8333-333333333333", "--json")
	require.NoError(t, err)

	listed, err := runCLI(t, "production", "policy", "list", "--limit", "1", "--json")
	require.NoError(t, err)
	var page api.ProductionPolicyPage
	require.NoError(t, json.Unmarshal([]byte(listed), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, stored.SHA256, page.Items[0].SHA256)
	require.NotEmpty(t, page.NextCursor)
	listed, err = runCLI(t, "production", "policy", "list", "--limit", "1",
		"--cursor", page.NextCursor, "--json")
	require.NoError(t, err)
	var next api.ProductionPolicyPage
	require.NoError(t, json.Unmarshal([]byte(listed), &next))
	require.Len(t, next.Items, 1)
	require.EqualValues(t, 2, next.Items[0].Version)
	require.Empty(t, next.NextCursor)

	shown, err := runCLI(t, "production", "policy", "show", policy.ID, "1", "--json")
	require.NoError(t, err)
	var exact documentproduction.PolicyVersion
	require.NoError(t, json.Unmarshal([]byte(shown), &exact))
	require.Equal(t, stored.SHA256, exact.SHA256)

	policy.Version = 1
	policy.Name = "Altered synthetic policy"
	encoded, err = json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, encoded, 0o600))
	_, err = runCLI(t, "production", "policy", "create", "--file", file,
		"--operation-id", operationID, "--json")
	require.Error(t, err)
	_, err = runCLI(t, "production", "policy", "list", "--limit", "101")
	require.ErrorContains(t, err, "--limit must be 1-100")
}
