package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionDraftCLIReadsPagesAndEditsInstructionsWithReplay(t *testing.T) {
	_ = setupVaultHome(t)
	createdJSON, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic draft", "--json")
	require.NoError(t, err)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(createdJSON), &created))
	setID := created.Set.ID
	revision := strconv.FormatInt(created.Draft.Revision, 10)
	etag := strconv.FormatInt(created.Draft.ETag, 10)

	membersJSON, err := runCLI(t, "production", "drafts", "members", setID, revision,
		"--limit", "1", "--json")
	require.NoError(t, err)
	var members api.ProductionMemberPage
	require.NoError(t, json.Unmarshal([]byte(membersJSON), &members))
	require.Empty(t, members.Items)
	decisionsJSON, err := runCLI(t, "production", "drafts", "decisions", setID, revision,
		"--limit", "1", "--uncertain", "true", "--json")
	require.NoError(t, err)
	var decisions api.ProductionDecisionPage
	require.NoError(t, json.Unmarshal([]byte(decisionsJSON), &decisions))
	require.Empty(t, decisions.Items)

	instructions := filepath.Join(t.TempDir(), "instructions.txt")
	require.NoError(t, os.WriteFile(instructions, []byte("Review synthetic pages only.\n"), 0o600))
	const operationID = "78000000-0000-4000-8000-000000000021"
	edit := []string{"production", "drafts", "instructions", setID, revision,
		"--etag", etag, "--instructions-file", instructions, "--operation-id", operationID, "--json"}
	editedJSON, err := runCLI(t, edit...)
	require.NoError(t, err)
	var receipt redaction.Receipt
	require.NoError(t, json.Unmarshal([]byte(editedJSON), &receipt))
	require.Equal(t, operationID, receipt.OperationID)
	require.Equal(t, created.Draft.ETag+1, receipt.ETag)
	replayedJSON, err := runCLI(t, edit...)
	require.NoError(t, err)
	require.JSONEq(t, editedJSON, replayedJSON)

	require.NoError(t, os.WriteFile(instructions, []byte("Changed synthetic instructions.\n"), 0o600))
	_, err = runCLI(t, edit...)
	require.ErrorContains(t, err, "production_operation_conflict")
	_, err = runCLI(t, "production", "drafts", "instructions", setID, revision,
		"--etag", etag, "--instructions-file", instructions,
		"--operation-id", "78000000-0000-4000-8000-000000000022")
	require.ErrorContains(t, err, "production_revision_conflict")
}

func TestProductionDraftCLIBoundsAreCheckedBeforeConnecting(t *testing.T) {
	_, err := runCLI(t, "production", "drafts", "members", "bad-set", "1", "--limit", "201")
	require.ErrorContains(t, err, "--limit must be 1-200")
	_, err = runCLI(t, "production", "drafts", "decisions", "bad-set", "1", "--limit", "501")
	require.ErrorContains(t, err, "--limit must be 1-500")
	_, err = runCLI(t, "production", "drafts", "decisions", "bad-set", "1", "--uncertain", "maybe")
	require.ErrorContains(t, err, "--uncertain must be all, true, or false")
	_, err = runCLI(t, "production", "drafts", "instructions", "bad-set", "1")
	require.ErrorContains(t, err, "--etag is required")
}
