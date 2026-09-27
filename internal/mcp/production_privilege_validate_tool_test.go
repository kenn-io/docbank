package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeValidateToolUsesStoredRowsAndReplays(t *testing.T) {
	var draft productiontest.PrivilegeDraft
	daemon := newProductionPolicyToolDaemon(t, func(s *store.Store) {
		draft = productiontest.SeedPrivilegeLogDraft(t, s)
	})
	require.Nil(t, catalogMap(toolCatalog(false))["validate_production_privilege_log"])
	tool := catalogMap(toolCatalog(true))["validate_production_privilege_log"]
	require.NotNil(t, tool)
	server := newBatesToolTestServer(t, daemon.URL, true)
	args := map[string]any{
		"log_id": draft.LogID, "revision": draft.Revision,
		"operation_id":        "edededed-eded-4ded-8ded-edededededed",
		"expected_generation": draft.Generation, "validated_at": "2026-09-22T14:00:00Z",
	}
	first := callToolResult(t, server, "validate_production_privilege_log", args)
	summary := objectField(t, first, "structuredContent")
	require.EqualValues(t, draft.Generation, summary["draft_generation"])
	require.NotEmpty(t, summary["inputs_sha256"])
	require.NotEmpty(t, summary["rows_sha256"])
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Synthetic private rationale.")
	require.NotContains(t, string(encoded), "person_ids")
	replay := callToolResult(t, server, "validate_production_privilege_log", args)
	require.Equal(t, summary["inputs_sha256"], objectField(t, replay, "structuredContent")["inputs_sha256"])
	args["expected_generation"] = draft.Generation + 1
	conflict := callToolResult(t, server, "validate_production_privilege_log", args)
	require.Equal(t, true, conflict["isError"])
	require.Equal(t, "production_privilege_conflict", objectField(t, conflict, "structuredContent")["code"])
	args["expected_generation"] = draft.Generation
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["validated_at"] = "2026-09-22T14:00:00+00:00"
	assertSchemaRejects(t, tool.InputSchema, args)
	args["validated_at"] = "not-a-time"
	assertSchemaRejects(t, tool.InputSchema, args)
}

func TestProductionPrivilegeValidateToolDoesNotRetryUnknownOutcome(t *testing.T) {
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := callToolResult(t, server, "validate_production_privilege_log", map[string]any{
		"log_id": "13131313-1313-4313-8313-131313131313", "revision": 1,
		"operation_id":        "edededed-eded-4ded-8ded-edededededed",
		"expected_generation": 1, "validated_at": "2026-09-22T14:00:00Z",
	})
	require.Equal(t, true, result["isError"])
	require.Equal(t, "production_privilege_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
