package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionSetCLIUsesDaemonAndReplaysExplicitOperation(t *testing.T) {
	_ = setupVaultHome(t)
	recipes, err := runCLI(t, "production", "recipes", "--json")
	require.NoError(t, err)
	var catalog api.ProductionRecipeCatalog
	require.NoError(t, json.Unmarshal([]byte(recipes), &catalog))
	require.Equal(t, redaction.DefaultRecipeID, catalog.DefaultID)
	require.Len(t, catalog.Items, 2)

	instructions := filepath.Join(t.TempDir(), "synthetic-instructions.txt")
	require.NoError(t, os.WriteFile(instructions, []byte("Review the synthetic phrase.\n"), 0o600))
	const operationID = "78000000-0000-4000-8000-000000000011"
	create := []string{"production", "sets", "create", "--name", "Synthetic review",
		"--instructions-file", instructions, "--operation-id", operationID, "--json"}
	created, err := runCLI(t, create...)
	require.NoError(t, err)
	var first api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(created), &first))
	require.Equal(t, "Synthetic review", first.Set.Name)
	require.Equal(t, first.Set.ID, first.Draft.SetID)
	require.Equal(t, int64(1), first.Draft.Revision)

	replayed, err := runCLI(t, create...)
	require.NoError(t, err)
	require.JSONEq(t, created, replayed)
	_, err = runCLI(t, "production", "sets", "create", "--name", "Changed payload",
		"--operation-id", operationID, "--json")
	require.ErrorContains(t, err, "production_operation_conflict")

	listed, err := runCLI(t, "production", "sets", "list", "--limit", "1", "--json")
	require.NoError(t, err)
	var page api.ProductionSetPage
	require.NoError(t, json.Unmarshal([]byte(listed), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, first.Set.ID, page.Items[0].ID)

	shown, err := runCLI(t, "production", "sets", "show", first.Set.ID, "--json")
	require.NoError(t, err)
	var set redaction.Set
	require.NoError(t, json.Unmarshal([]byte(shown), &set))
	require.Equal(t, first.Set, set)

	draftJSON, err := runCLI(t, "production", "drafts", "show", first.Set.ID, "1", "--json")
	require.NoError(t, err)
	var draft redaction.Draft
	require.NoError(t, json.Unmarshal([]byte(draftJSON), &draft))
	require.Equal(t, first.Draft, draft)
}

func TestProductionSetCLIRejectsOversizedInstructionsAndUnboundedPages(t *testing.T) {
	instructions := filepath.Join(t.TempDir(), "too-large.txt")
	require.NoError(t, os.WriteFile(instructions,
		[]byte(strings.Repeat("x", redaction.MaxInstructionsBytes+1)), 0o600))
	_, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic review",
		"--instructions-file", instructions)
	require.ErrorContains(t, err, "instructions exceed")
	_, err = runCLI(t, "production", "sets", "list", "--limit", "201")
	require.ErrorContains(t, err, "--limit must be 1-200")
	_, err = runCLI(t, "production", "drafts", "show", "bad-set", "0")
	require.ErrorContains(t, err, "revision must be a positive integer")
}

func TestProductionSetCLIGeneratesFreshOperationForEachCommand(t *testing.T) {
	_ = setupVaultHome(t)
	firstJSON, err := runCLI(t, "production", "sets", "create", "--name", "First", "--json")
	require.NoError(t, err)
	secondJSON, err := runCLI(t, "production", "sets", "create", "--name", "Second", "--json")
	require.NoError(t, err)
	var first, second api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(firstJSON), &first))
	require.NoError(t, json.Unmarshal([]byte(secondJSON), &second))
	require.NotEqual(t, first.Set.ID, second.Set.ID)
}
