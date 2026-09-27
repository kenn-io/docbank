package main

import (
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionDraftForkCLIReplaysAndStartsEditableRevision(t *testing.T) {
	_ = setupVaultHome(t)
	createdJSON, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic fork", "--json")
	require.NoError(t, err)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(createdJSON), &created))
	const operationID = "78000000-0000-4000-8000-000000000041"
	fork := []string{"production", "drafts", "fork", created.Set.ID, "1", "--operation-id", operationID, "--json"}
	forkedJSON, err := runCLI(t, fork...)
	require.NoError(t, err)
	var forked redaction.Draft
	require.NoError(t, json.Unmarshal([]byte(forkedJSON), &forked))
	require.EqualValues(t, 2, forked.Revision)
	require.EqualValues(t, 1, forked.ETag)
	require.Equal(t, "draft", forked.State)
	require.False(t, forked.MembershipSealed)
	require.Equal(t, created.Draft.MemberHash, forked.MemberHash)

	replayedJSON, err := runCLI(t, fork...)
	require.NoError(t, err)
	require.JSONEq(t, forkedJSON, replayedJSON)
	_, err = runCLI(t, "production", "drafts", "fork", created.Set.ID,
		strconv.FormatInt(forked.Revision, 10), "--operation-id", operationID)
	require.ErrorContains(t, err, "production_operation_conflict")

	shownJSON, err := runCLI(t, "production", "drafts", "show", created.Set.ID, "2", "--json")
	require.NoError(t, err)
	require.JSONEq(t, forkedJSON, shownJSON)
	_, err = runCLI(t, "production", "drafts", "fork", created.Set.ID, "2", "--json")
	require.NoError(t, err)
	setJSON, err := runCLI(t, "production", "sets", "show", created.Set.ID, "--json")
	require.NoError(t, err)
	var set redaction.Set
	require.NoError(t, json.Unmarshal([]byte(setJSON), &set))
	require.EqualValues(t, 3, set.HeadRevision)
}

func TestProductionDraftForkCLIRejectsInvalidRevisionBeforeConnecting(t *testing.T) {
	_, err := runCLI(t, "production", "drafts", "fork", "bad-set", "0")
	require.ErrorContains(t, err, "revision must be a positive integer")
}
