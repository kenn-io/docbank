package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
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

func TestProductionPrivilegeCLIDraftsPrivateRowsThroughDaemonClient(t *testing.T) {
	const logID = "21212121-2121-4121-8121-212121212121"
	request := api.ProductionPrivilegeDraftCreateRequest{
		OperationID: "22222222-2222-4222-8222-222222222222",
		SetID:       "23232323-2323-4232-8232-232323232323", SetRevision: 1,
		PlayersSHA256: strings.Repeat("a", 64),
		Rows: []documentproduction.PrivilegeRow{{
			ID:               "24242424-2424-4242-8242-242424242424",
			WithheldMemberID: "25252525-2525-4252-8252-252525252525",
			FamilyOrder:      1, SourceVersionID: "26262626-2626-4262-8262-262626262626",
			Basis: "synthetic_basis", PublicDescription: "Synthetic description.",
			PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: strings.Repeat("b", 64),
			PersonIDs: []string{"27272727-2727-4272-8272-272727272727"},
			Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}},
		}},
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/production-privilege-logs/"+logID+"/revisions/1/draft", r.URL.Path)
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
		var actual api.ProductionPrivilegeDraftCreateRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, request, actual)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, api.ProductionPrivilegeDraftGeneration{
			LogID: logID, Revision: 1, Generation: 1,
		}))
	}))
	t.Cleanup(server.Close)
	rowsJSON, err := json.Marshal(request.Rows)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "private-draft-rows.json")
	require.NoError(t, os.WriteFile(file, rowsJSON, 0o600))
	var ensureCalls int
	cmd := newProductionPrivilegeDraftCommandWithEnsure(func(context.Context) (*daemonconn.Connection, error) {
		ensureCalls++
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{logID, "1", "--operation-id", request.OperationID,
		"--set-id", request.SetID, "--set-revision", "1",
		"--players-sha256", request.PlayersSHA256, "--file", file, "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	var created api.ProductionPrivilegeDraftGeneration
	require.NoError(t, json.Unmarshal(out.Bytes(), &created))
	require.Equal(t, int64(1), created.Generation)
	require.NotContains(t, out.String(), request.Rows[0].PrivateRationale)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, ensureCalls)
	require.NoError(t, os.WriteFile(file, []byte(`[{"unexpected_private_field":true}]`), 0o600))
	bad := newProductionPrivilegeDraftCommandWithEnsure(func(context.Context) (*daemonconn.Connection, error) {
		ensureCalls++
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	})
	bad.SetArgs([]string{logID, "1", "--operation-id", request.OperationID,
		"--set-id", request.SetID, "--set-revision", "1",
		"--players-sha256", request.PlayersSHA256, "--file", file})
	require.ErrorContains(t, bad.ExecuteContext(t.Context()), "invalid privilege rows JSON")
	require.Equal(t, 1, ensureCalls)

	help, err := runCLI(t, "production", "privilege-log", "draft", "--help")
	require.NoError(t, err)
	require.Contains(t, help, "--set-id")
	require.Contains(t, help, "--players-sha256")
	require.Contains(t, help, "--file")
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
