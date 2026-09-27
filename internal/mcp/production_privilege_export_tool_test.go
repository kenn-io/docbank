package mcp

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeExportToolPublishesVerifiedPublicFile(t *testing.T) {
	daemon := newProductionPolicyToolDaemon(t, func(s *store.Store) {
		productiontest.SeedFrozenPrivilegeLog(t, s)
	})
	require.Nil(t, catalogMap(toolCatalog(false))["export_production_privilege_log"])
	tool := catalogMap(toolCatalog(true))["export_production_privilege_log"]
	require.NotNil(t, tool)
	server := newBatesToolTestServer(t, daemon.URL, true)
	const logID = "13131313-1313-4313-8313-131313131313"
	path := filepath.Join(t.TempDir(), "privilege.csv")
	args := map[string]any{"log_id": logID, "revision": 1, "format": "csv",
		"destination_path": path, "overwrite": false}
	result := callToolResult(t, server, "export_production_privilege_log", args)
	summary := objectField(t, result, "structuredContent")
	require.Equal(t, path, summary["destination_path"])
	require.Equal(t, "published", summary["state"])
	require.NotEmpty(t, summary["content_sha256"])
	require.NotEmpty(t, summary["receipt_sha256"])
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "Synthetic public description.")
	require.NotContains(t, string(content), "Synthetic private rationale.")
	require.NotContains(t, string(content), "synthetic@example.test")
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Synthetic private rationale.")
	again := callToolResult(t, server, "export_production_privilege_log", args)
	require.Equal(t, true, again["isError"])
	require.Equal(t, "destination_exists", objectField(t, again, "structuredContent")["code"])
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, unchanged)
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["format"] = "html"
	assertSchemaRejects(t, tool.InputSchema, args)
}
