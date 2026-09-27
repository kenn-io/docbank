package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
)

func syntheticMCPPrivilegeRowsFile(t *testing.T) (string, []documentproduction.PrivilegeRow) {
	t.Helper()
	rows := []documentproduction.PrivilegeRow{{
		ID:               "31313131-3131-4313-8313-313131313131",
		WithheldMemberID: "32323232-3232-4323-8323-323232323232",
		FamilyOrder:      1, SourceVersionID: "33333333-3333-4333-8333-333333333333",
		Basis: "synthetic_basis", PublicDescription: "Synthetic public description.",
		PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: strings.Repeat("a", 64),
		PersonIDs: []string{"34343434-3434-4343-8343-343434343434"},
		Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}},
	}}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "private-draft-rows.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	return file, rows
}

func TestProductionPrivilegeDraftToolUsesPrivateFileAndSafeReceipt(t *testing.T) {
	const logID = "35353535-3535-4353-8353-353535353535"
	file, rows := syntheticMCPPrivilegeRowsFile(t)
	request := api.ProductionPrivilegeDraftCreateRequest{
		OperationID: "36363636-3636-4363-8363-363636363636",
		SetID:       "37373737-3737-4373-8373-373737373737", SetRevision: 1,
		PlayersSHA256: strings.Repeat("b", 64), Rows: rows,
	}
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/production-privilege-logs/"+logID+"/revisions/1/draft", r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		var actual api.ProductionPrivilegeDraftCreateRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, request, actual)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, api.ProductionPrivilegeDraftGeneration{
			LogID: logID, Revision: 1, Generation: 1,
		}))
	}))
	t.Cleanup(daemon.Close)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_privilege_draft"])
	tool := catalogMap(toolCatalog(true))["create_production_privilege_draft"]
	require.NotNil(t, tool)
	server := newBatesToolTestServer(t, daemon.URL, true)
	args := map[string]any{
		"log_id": logID, "revision": 1, "operation_id": request.OperationID,
		"set_id": request.SetID, "set_revision": 1,
		"players_sha256": request.PlayersSHA256, "rows_file": file,
	}
	first := callToolResult(t, server, "create_production_privilege_draft", args)
	summary := objectField(t, first, "structuredContent")
	require.Equal(t, logID, summary["log_id"])
	require.EqualValues(t, 1, summary["generation"])
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), rows[0].PrivateRationale)
	second := callToolResult(t, server, "create_production_privilege_draft", args)
	require.Equal(t, summary["generation"], objectField(t, second, "structuredContent")["generation"])
	require.Equal(t, 2, calls)
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["predecessor_log_id"] = "38383838-3838-4383-8383-383838383838"
	bad := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "create_production_privilege_draft", "arguments": args,
	})))
	require.NotContains(t, bad.Message, rows[0].PrivateRationale)
	require.Equal(t, 2, calls)
}

func TestProductionPrivilegeDraftToolDoesNotRetryUnknownOutcome(t *testing.T) {
	file, _ := syntheticMCPPrivilegeRowsFile(t)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := callToolResult(t, server, "create_production_privilege_draft", map[string]any{
		"log_id": "35353535-3535-4353-8353-353535353535", "revision": 1,
		"operation_id": "36363636-3636-4363-8363-363636363636",
		"set_id":       "37373737-3737-4373-8373-373737373737", "set_revision": 1,
		"players_sha256": strings.Repeat("b", 64), "rows_file": file,
	})
	require.Equal(t, true, result["isError"])
	require.Equal(t, "production_privilege_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
