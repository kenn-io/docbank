package main

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionReviewCLIRoutesAreRevisionFenced(t *testing.T) {
	_ = setupVaultHome(t)
	createdJSON, err := runCLI(t, "production", "sets", "create", "--name", "Synthetic review gates", "--json")
	require.NoError(t, err)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(createdJSON), &created))
	setID := created.Set.ID
	revision := strconv.FormatInt(created.Draft.Revision, 10)
	etag := strconv.FormatInt(created.Draft.ETag, 10)
	const memberID = "78000000-0000-4000-8000-000000000051"
	const operationID = "78000000-0000-4000-8000-000000000052"

	_, err = runCLI(t, "production", "drafts", "resolve", setID, revision, memberID, "1",
		"--etag", etag, "--limit", "1", "--json")
	require.ErrorContains(t, err, "not found") // The real daemon has no retained member to resolve.
	_, err = runCLI(t, "production", "drafts", "review", setID, revision, memberID,
		"--etag", etag, "--binding", strings.Repeat("a", 64), "--operation-id", operationID)
	require.ErrorContains(t, err, "invalid_production") // Review must not accept a caller-invented binding.
	_, err = runCLI(t, "production", "drafts", "seal", setID, revision,
		"--etag", etag, "--total", "1", "--member-hash", created.Draft.MemberHash,
		"--operation-id", operationID)
	require.ErrorContains(t, err, "production_") // The declared count disagrees with the empty draft.

	shownJSON, err := runCLI(t, "production", "drafts", "show", setID, revision, "--json")
	require.NoError(t, err)
	var shown redaction.Draft
	require.NoError(t, json.Unmarshal([]byte(shownJSON), &shown))
	require.Equal(t, created.Draft.ETag, shown.ETag)
	require.False(t, shown.MembershipSealed)
}

func TestProductionReviewCLIBoundsBeforeConnecting(t *testing.T) {
	_, err := runCLI(t, "production", "drafts", "seal", "bad-set", "1")
	require.ErrorContains(t, err, "--etag is required")
	_, err = runCLI(t, "production", "drafts", "seal", "bad-set", "1",
		"--etag", "1", "--total", "0", "--member-hash", strings.Repeat("a", 64))
	require.ErrorContains(t, err, "invalid production membership seal")
	_, err = runCLI(t, "production", "drafts", "resolve", "bad-set", "1", "bad-member", "1",
		"--etag", "1", "--limit", "201")
	require.ErrorContains(t, err, "--limit must be 1-200")
	_, err = runCLI(t, "production", "drafts", "resolve", "bad-set", "1", "bad-member", "0",
		"--etag", "1")
	require.ErrorContains(t, err, "page must be a positive integer")
	_, err = runCLI(t, "production", "drafts", "review", "bad-set", "1", "bad-member",
		"--etag", "1", "--binding", "bad-hash")
	require.ErrorContains(t, err, "--binding must be a SHA-256 hex digest")
}
