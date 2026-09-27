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

func TestProductionPrivilegeFreezeToolValidatesThenFreezesThroughDaemon(t *testing.T) {
	var draft productiontest.PrivilegeDraft
	daemon := newProductionPolicyToolDaemon(t, func(s *store.Store) {
		draft = productiontest.SeedPrivilegeLogDraft(t, s)
	})
	require.Nil(t, catalogMap(toolCatalog(false))["freeze_production_privilege_log"])
	tool := catalogMap(toolCatalog(true))["freeze_production_privilege_log"]
	require.NotNil(t, tool)
	server := newBatesToolTestServer(t, daemon.URL, true)
	validated := callToolResult(t, server, "validate_production_privilege_log", map[string]any{
		"log_id": draft.LogID, "revision": draft.Revision,
		"operation_id":        "43434343-4343-4434-8434-434343434343",
		"expected_generation": draft.Generation, "validated_at": "2026-09-22T14:00:00Z",
	})
	validation := objectField(t, validated, "structuredContent")
	require.NotEmpty(t, validation["inputs_sha256"])
	args := map[string]any{
		"log_id": draft.LogID, "revision": draft.Revision,
		"operation_id":           "44444444-4444-4444-8444-444444444444",
		"expected_generation":    draft.Generation,
		"expected_inputs_sha256": validation["inputs_sha256"],
		"frozen_at":              "2026-09-22T14:01:00Z",
	}
	first := callToolResult(t, server, "freeze_production_privilege_log", args)
	summary := objectField(t, first, "structuredContent")
	require.Equal(t, draft.LogID, summary["log_id"])
	require.EqualValues(t, len(draft.Rows), summary["row_count"])
	require.NotEmpty(t, summary["receipt_sha256"])
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Synthetic private rationale.")
	replay := callToolResult(t, server, "freeze_production_privilege_log", args)
	require.Equal(t, summary["receipt_sha256"], objectField(t, replay, "structuredContent")["receipt_sha256"])
	args["frozen_at"] = "2026-09-22T14:02:00Z"
	conflict := callToolResult(t, server, "freeze_production_privilege_log", args)
	require.Equal(t, true, conflict["isError"])
	require.Equal(t, "production_privilege_conflict", objectField(t, conflict, "structuredContent")["code"])
	args["frozen_at"] = "2026-09-22T14:01:00Z"
	assertSchemaAccepts(t, tool.InputSchema, args)
	args["frozen_at"] = "2026-09-22T14:01:00+00:00"
	assertSchemaRejects(t, tool.InputSchema, args)
}

func TestProductionPrivilegeFreezeToolPreservesApprovalCodesAndUnknownOutcome(t *testing.T) {
	args := map[string]any{
		"log_id": "13131313-1313-4313-8313-131313131313", "revision": 1,
		"operation_id":           "45454545-4545-4454-8454-454545454545",
		"expected_generation":    1,
		"expected_inputs_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"frozen_at":              "2026-09-22T14:01:00Z",
	}
	for _, code := range []string{"approval_required", "approval_stale"} {
		t.Run(code, func(t *testing.T) {
			var calls int
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"status":409,"code":"` + code + `","detail":"Synthetic private approval evidence."}`))
			}))
			t.Cleanup(daemon.Close)
			server := newBatesToolTestServer(t, daemon.URL, true)
			result := callToolResult(t, server, "freeze_production_privilege_log", args)
			require.Equal(t, true, result["isError"])
			require.Equal(t, code, objectField(t, result, "structuredContent")["code"])
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "Synthetic private approval evidence.")
			require.Equal(t, 1, calls)
		})
	}
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := callToolResult(t, server, "freeze_production_privilege_log", args)
	require.Equal(t, true, result["isError"])
	require.Equal(t, "production_privilege_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
