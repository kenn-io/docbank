package mcp

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeReadToolPagesFrozenPublicRows(t *testing.T) {
	daemon := newProductionPolicyToolDaemon(t, func(s *store.Store) {
		productiontest.SeedFrozenPrivilegeLog(t, s)
	})
	server := newBatesToolTestServer(t, daemon.URL, false)
	require.NotNil(t, catalogMap(toolCatalog(false))["get_production_privilege_log"])
	const logID = "13131313-1313-4313-8313-131313131313"
	first := callToolResult(t, server, "get_production_privilege_log", map[string]any{
		"log_id": logID, "revision": 1, "limit": 1,
	})
	page := objectField(t, first, "structuredContent")
	require.Equal(t, logID, objectField(t, page, "receipt")["log_id"])
	require.Len(t, page["rows"], 1)
	require.Equal(t, "1", page["next_cursor"])
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	for _, private := range []string{"Synthetic private rationale.", "person_ids", "evidence_sha256", "fields"} {
		require.NotContains(t, string(encoded), private)
	}
	second := callToolResult(t, server, "get_production_privilege_log", map[string]any{
		"log_id": logID, "revision": 1, "limit": 1, "cursor": page["next_cursor"],
	})
	secondPage := objectField(t, second, "structuredContent")
	require.Len(t, secondPage["rows"], 1)
	require.Empty(t, secondPage["next_cursor"])
	tool := catalogMap(toolCatalog(false))["get_production_privilege_log"]
	assertSchemaAccepts(t, tool.InputSchema, map[string]any{"log_id": logID, "revision": 1, "limit": 1})
	assertSchemaRejects(t, tool.InputSchema, map[string]any{"log_id": logID, "revision": 1, "limit": 101})
}
