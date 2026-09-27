package main

import (
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionJobsCLIGatesUnfinalizedDraft(t *testing.T) {
	_ = setupVaultHome(t)
	createdJSON, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic job gate", "--json")
	require.NoError(t, err)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(createdJSON), &created))
	setID := created.Set.ID
	revision := strconv.FormatInt(created.Draft.Revision, 10)
	etag := strconv.FormatInt(created.Draft.ETag, 10)
	const namespaceID = "78000000-0000-4000-8000-000000000061"
	const snapshotID = "78000000-0000-4000-8000-000000000062"
	const jobID = "78000000-0000-4000-8000-000000000063"
	const operationID = "78000000-0000-4000-8000-000000000064"

	_, err = runCLI(t, "production", "drafts", "finalize", setID, revision,
		"--etag", etag, "--namespace-id", namespaceID, "--snapshot-id", snapshotID,
		"--operation-id", operationID)
	require.ErrorContains(t, err, "production") // The draft has no reviewed members or valid numbering snapshot.
	_, err = runCLI(t, "production", "jobs", "admit", setID, revision,
		"--etag", etag, "--job-id", jobID, "--operation-id", operationID)
	require.ErrorContains(t, err, "production") // An unfinalized draft cannot enter production.
	_, err = runCLI(t, "production", "jobs", "status", setID, jobID)
	require.ErrorContains(t, err, "not found") // Failed admission did not create a job.
	_, err = runCLI(t, "production", "jobs", "cancel", setID, jobID,
		"--etag", etag, "--operation-id", operationID)
	require.ErrorContains(t, err, "not found")

	shownJSON, err := runCLI(t, "production", "drafts", "show", setID, revision, "--json")
	require.NoError(t, err)
	var draft redaction.Draft
	require.NoError(t, json.Unmarshal([]byte(shownJSON), &draft))
	require.Equal(t, created.Draft.ETag, draft.ETag)
	require.Equal(t, "draft", draft.State)
}

func TestProductionJobsCLIBoundsBeforeConnecting(t *testing.T) {
	_, err := runCLI(t, "production", "drafts", "finalize", "bad-set", "1")
	require.ErrorContains(t, err, "--etag is required")
	_, err = runCLI(t, "production", "drafts", "finalize", "bad-set", "1", "--etag", "1")
	require.ErrorContains(t, err, "--namespace-id and --snapshot-id are required")
	_, err = runCLI(t, "production", "drafts", "finalize", "bad-set", "1",
		"--etag", "1", "--namespace-id", "bad", "--snapshot-id", "bad", "--start-at", "-1")
	require.ErrorContains(t, err, "--start-at must be nonnegative")
	_, err = runCLI(t, "production", "jobs", "admit", "bad-set", "1")
	require.ErrorContains(t, err, "--etag is required")
	_, err = runCLI(t, "production", "jobs", "cancel", "bad-set", "bad-job")
	require.ErrorContains(t, err, "--etag is required")
}
