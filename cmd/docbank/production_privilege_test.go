package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeCLIExportsPublicFrozenBytesFromRealDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	productiontest.SeedFrozenPrivilegeLog(t, catalog)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)

	const logID = "13131313-1313-4313-8313-131313131313"
	destination := filepath.Join(t.TempDir(), "privilege.csv")
	args := []string{"production", "privilege-log", "export", logID, "1", "csv", destination}
	out, err := runCLI(t, args...)
	require.NoError(t, err)
	require.Contains(t, out, destination)
	require.Contains(t, out, "SHA-256")
	content, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Contains(t, string(content), "Synthetic public description.")
	require.NotContains(t, string(content), "Synthetic private rationale.")
	require.NotContains(t, string(content), "synthetic@example.test")
	_, err = runCLI(t, args...)
	require.Error(t, err)
	again, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, content, again)
}

func TestProductionPrivilegeCLIReadsFrozenPublicPagesFromRealDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	productiontest.SeedFrozenPrivilegeLog(t, catalog)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)

	const logID = "13131313-1313-4313-8313-131313131313"
	out, err := runCLI(t, "production", "privilege-log", "show", logID, "1", "--limit", "1", "--json")
	require.NoError(t, err)
	var first api.ProductionPrivilegePublicPage
	require.NoError(t, json.Unmarshal([]byte(out), &first))
	require.Equal(t, logID, first.Receipt.LogID)
	require.Len(t, first.Rows, 1)
	require.Equal(t, "1", first.NextCursor)
	require.NotContains(t, out, "Synthetic private rationale.")
	require.NotContains(t, out, "person_ids")
	out, err = runCLI(t, "production", "privilege-log", "show", logID, "1",
		"--limit", "1", "--cursor", first.NextCursor, "--json")
	require.NoError(t, err)
	var second api.ProductionPrivilegePublicPage
	require.NoError(t, json.Unmarshal([]byte(out), &second))
	require.Len(t, second.Rows, 1)
	require.Equal(t, "Second synthetic public description.", second.Rows[0].PublicDescription)
	require.Empty(t, second.NextCursor)
	_, err = runCLI(t, "production", "privilege-log", "show", logID, "1", "--limit", "101")
	require.Error(t, err)
}

func TestProductionPrivilegeCLIValidatesStoredDraftFromRealDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	draft := productiontest.SeedPrivilegeLogDraft(t, catalog)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)
	args := []string{"production", "privilege-log", "validate", draft.LogID, "1",
		"--operation-id", "edededed-eded-4ded-8ded-edededededed", "--generation", "1",
		"--validated-at", "2026-09-22T14:00:00Z", "--json"}
	out, err := runCLI(t, args...)
	require.NoError(t, err)
	var validated api.ProductionPrivilegeValidation
	require.NoError(t, json.Unmarshal([]byte(out), &validated))
	require.Equal(t, draft.Generation, validated.DraftGeneration)
	require.Equal(t, draft.LogID, validated.Validation.Inputs.LogID)
	require.NotContains(t, out, "Synthetic private rationale.")
	replay, err := runCLI(t, args...)
	require.NoError(t, err)
	require.Equal(t, out, replay)
	args[8] = "2"
	_, err = runCLI(t, args...)
	require.Error(t, err)
}

func TestProductionPrivilegeCLIReplacesPrivateRowsThroughRealDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	draft := productiontest.SeedPrivilegeLogDraft(t, catalog)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)
	rows := slices.Clone(draft.Rows)
	rows[0].PublicDescription = "Updated synthetic public description."
	rows[0].PrivateRationale = "Updated synthetic private rationale."
	file := filepath.Join(t.TempDir(), "private-rows.json")
	writeRows := func() {
		t.Helper()
		raw, err := json.Marshal(rows)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, raw, 0o600))
	}
	writeRows()
	args := []string{"production", "privilege-log", "rows", "replace", draft.LogID, "1",
		"--operation-id", "16161616-1616-4616-8616-161616161616",
		"--generation", "1", "--file", file, "--json"}
	out, err := runCLI(t, args...)
	require.NoError(t, err)
	var replaced api.ProductionPrivilegeDraftGeneration
	require.NoError(t, json.Unmarshal([]byte(out), &replaced))
	require.Equal(t, draft.Generation+1, replaced.Generation)
	require.NotContains(t, out, rows[0].PrivateRationale)
	replay, err := runCLI(t, args...)
	require.NoError(t, err)
	require.Equal(t, out, replay)
	rows[0].PrivateRationale = "Changed again."
	writeRows()
	_, err = runCLI(t, args...)
	require.Error(t, err)
	args[7] = "17171717-1717-4717-8717-171717171717"
	_, err = runCLI(t, args...)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(file, []byte(`[{"unexpected_private_field":true}]`), 0o600))
	_, err = runCLI(t, args...)
	require.ErrorContains(t, err, "invalid privilege rows JSON")
}
