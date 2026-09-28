package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionGateSelectionToolPinsExactStoredReferences(t *testing.T) {
	const setID = "7a000000-0000-4000-8000-000000000041"
	const approvalID = "7a000000-0000-4000-8000-000000000042"
	const logID = "7a000000-0000-4000-8000-000000000043"
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "/api/v1/productions/sets/"+setID+"/revisions/2/gate-authority", r.URL.Path)
		var body api.ProductionGateSelectionRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Equal(t, approvalID, body.ApprovalID)
		assert.Equal(t, logID, body.PrivilegeLogID)
		assert.EqualValues(t, 1, body.PrivilegeLogRevision)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(daemon.Close)
	require.Nil(t, catalogMap(toolCatalog(false))["select_production_gate_authority"])
	tool := catalogMap(toolCatalog(true))["select_production_gate_authority"]
	require.NotNil(t, tool)
	args := map[string]any{"set_id": setID, "revision": 2, "approval_id": approvalID,
		"privilege_log_id": logID, "privilege_log_revision": 1}
	assertSchemaAccepts(t, tool.InputSchema, args)
	server := newBatesToolTestServer(t, daemon.URL, true)
	first := callToolResult(t, server, "select_production_gate_authority", args)
	require.NotEqual(t, true, first["isError"], first)
	result := objectField(t, first, "structuredContent")
	require.Equal(t, setID, result["set_id"])
	require.Equal(t, approvalID, result["approval_id"])
	require.Equal(t, logID, result["privilege_log_id"])
	require.EqualValues(t, 1, result["privilege_log_revision"])
	second := callToolResult(t, server, "select_production_gate_authority", args)
	require.Equal(t, result, objectField(t, second, "structuredContent"))
	require.Equal(t, 2, calls)
	args["approval_id"] = "not-a-uuid"
	assertSchemaRejects(t, tool.InputSchema, args)
}

func TestProductionGateSelectionToolReportsUnknownOutcomeAfterLostResponse(t *testing.T) {
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack its response")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijacking test response: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := callToolResult(t, server, "select_production_gate_authority", map[string]any{
		"set_id": "7a000000-0000-4000-8000-000000000041", "revision": 2,
	})
	require.Equal(t, true, result["isError"])
	require.Equal(t, "production_gate_selection_outcome_unknown",
		objectField(t, result, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
