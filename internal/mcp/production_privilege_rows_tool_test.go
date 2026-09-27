package mcp

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeRowsToolReplacesPrivateRowsThroughRealDaemon(t *testing.T) {
	var draft productiontest.PrivilegeDraft
	daemon := newProductionPolicyToolDaemon(t, func(s *store.Store) {
		draft = productiontest.SeedPrivilegeLogDraft(t, s)
	})
	require.Nil(t, catalogMap(toolCatalog(false))["replace_production_privilege_rows"])
	tool := catalogMap(toolCatalog(true))["replace_production_privilege_rows"]
	require.NotNil(t, tool)
	server := newBatesToolTestServer(t, daemon.URL, true)
	rows := slices.Clone(draft.Rows)
	rows[0].PrivateRationale = "Updated synthetic private rationale."
	file := filepath.Join(t.TempDir(), "private-rows.json")
	writeRows := func() {
		t.Helper()
		raw, err := json.Marshal(rows)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, raw, 0o600))
	}
	writeRows()
	args := map[string]any{
		"log_id": draft.LogID, "revision": draft.Revision,
		"operation_id":        "28282828-2828-4282-8282-282828282828",
		"expected_generation": draft.Generation, "rows_file": file,
	}
	first := callToolResult(t, server, "replace_production_privilege_rows", args)
	summary := objectField(t, first, "structuredContent")
	require.Equal(t, draft.LogID, summary["log_id"])
	require.EqualValues(t, draft.Generation+1, summary["generation"])
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), rows[0].PrivateRationale)
	replay := callToolResult(t, server, "replace_production_privilege_rows", args)
	require.Equal(t, summary["generation"], objectField(t, replay, "structuredContent")["generation"])
	rows[0].PrivateRationale = "Changed synthetic private rationale."
	writeRows()
	conflict := callToolResult(t, server, "replace_production_privilege_rows", args)
	require.Equal(t, true, conflict["isError"])
	require.Equal(t, "production_privilege_conflict", objectField(t, conflict, "structuredContent")["code"])
	require.NoError(t, os.WriteFile(file, []byte(`[{"unexpected_private_field":true}]`), 0o600))
	bad := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "replace_production_privilege_rows", "arguments": args,
	})))
	require.NotContains(t, bad.Message, rows[0].PrivateRationale)
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["rows_file"] = "private-rows.json"
	badPath := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "replace_production_privilege_rows", "arguments": args,
	})))
	require.NotContains(t, badPath.Message, rows[0].PrivateRationale)
}
