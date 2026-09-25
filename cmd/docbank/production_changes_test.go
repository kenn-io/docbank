package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionDraftChangesCLIReplaysAndFencesRevision(t *testing.T) {
	_ = setupVaultHome(t)
	createdJSON, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic changes", "--json")
	require.NoError(t, err)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(createdJSON), &created))
	setID := created.Set.ID
	revision := strconv.FormatInt(created.Draft.Revision, 10)
	etag := strconv.FormatInt(created.Draft.ETag, 10)
	changesFile := filepath.Join(t.TempDir(), "changes.json")
	require.NoError(t, os.WriteFile(changesFile,
		[]byte(`[{"kind":"recipe","recipe_id":"raster-redaction/v1-600dpi"}]`), 0o600))
	const operationID = "78000000-0000-4000-8000-000000000031"
	apply := []string{"production", "drafts", "changes", setID, revision, "--etag", etag,
		"--changes-file", changesFile, "--operation-id", operationID, "--json"}
	appliedJSON, err := runCLI(t, apply...)
	require.NoError(t, err)
	var receipt redaction.Receipt
	require.NoError(t, json.Unmarshal([]byte(appliedJSON), &receipt))
	require.Equal(t, operationID, receipt.OperationID)
	require.Equal(t, created.Draft.ETag+1, receipt.ETag)
	draftJSON, err := runCLI(t, "production", "drafts", "show", setID, revision, "--json")
	require.NoError(t, err)
	var draft redaction.Draft
	require.NoError(t, json.Unmarshal([]byte(draftJSON), &draft))
	require.Equal(t, redaction.RecipeID600DPI, draft.RecipeID)

	replayedJSON, err := runCLI(t, apply...)
	require.NoError(t, err)
	require.JSONEq(t, appliedJSON, replayedJSON)
	require.NoError(t, os.WriteFile(changesFile,
		[]byte(`[{"kind":"recipe","recipe_id":"raster-redaction/v1-300dpi"}]`), 0o600))
	_, err = runCLI(t, apply...)
	require.ErrorContains(t, err, "production_operation_conflict")
	_, err = runCLI(t, "production", "drafts", "changes", setID, revision, "--etag", etag,
		"--changes-file", changesFile, "--operation-id", "78000000-0000-4000-8000-000000000032")
	require.ErrorContains(t, err, "production_revision_conflict")
}

func TestProductionDraftChangesCLIBoundsInputBeforeConnecting(t *testing.T) {
	changesFile := filepath.Join(t.TempDir(), "changes.json")
	_, err := runCLI(t, "production", "drafts", "changes", "bad-set", "1")
	require.ErrorContains(t, err, "--etag is required")
	require.NoError(t, os.WriteFile(changesFile, []byte(`[{"kind":"recipe","unknown":true}]`), 0o600))
	_, err = runCLI(t, "production", "drafts", "changes", "bad-set", "1", "--etag", "1",
		"--changes-file", changesFile)
	require.ErrorContains(t, err, "invalid production changes JSON")
	require.NoError(t, os.WriteFile(changesFile,
		[]byte(strings.Repeat("x", redaction.MaxCommandBytes+1)), 0o600))
	_, err = runCLI(t, "production", "drafts", "changes", "bad-set", "1", "--etag", "1",
		"--changes-file", changesFile)
	require.ErrorContains(t, err, "changes exceed")
}
