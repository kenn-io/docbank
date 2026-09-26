package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestAgentCapabilityToolDiscoversReviewedDaemonOperation(t *testing.T) {
	operation := agentops.CurrentOperations()[0]
	for _, candidate := range agentops.CurrentOperations() {
		if candidate.ID == "list_tags" {
			operation = candidate
			break
		}
	}
	assert.True(t, operation.ReviewedBehavior())
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agent/capabilities", r.URL.Path)
		assert.Equal(t, "synthetic-session", r.Header.Get(api.AgentSessionHeader))
		assert.Empty(t, r.Header.Get("X-Api-Key"))
		writeDaemonJSON(t, w, api.AgentCapabilities{Contract: agentops.Schema, Server: agentops.ServerCapabilities{
			VaultID: testVaultID, Version: "v1", RegistryDigest: "sha256:" + strings.Repeat("a", 64),
			Routes:     []agentops.Route{{ID: "GET /api/v1/tags", Method: "GET", Pattern: "/api/v1/tags", Class: agentops.Read}},
			Operations: []agentops.Operation{operation},
			Features:   []agentops.Feature{{Name: "native_documents", State: "available"}},
		}})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.AttachAgentSession(daemon.URL, "synthetic-session")
	}, func(*daemonconn.Connection) error { return nil })
	tool := catalogMap(toolCatalog(false, false, false))["get_agent_capabilities"]
	require.NotNil(t, tool)
	result, err := invokeReadTool(t.Context(), lease, "get_agent_capabilities", map[string]any{})
	require.NoError(t, err)
	output := structuredMap(t, result.StructuredContent)
	assertSchemaAccepts(t, tool.OutputSchema, output)
	assert.Equal(t, agentops.Schema, output["schema"])
	assert.Equal(t, "private", output["cacheScope"])
	operations, ok := output["operations"].([]any)
	require.True(t, ok)
	require.Len(t, operations, 1)
	entry, ok := operations[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "list_tags", entry["id"])
}
