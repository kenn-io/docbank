package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestMissLogEmptyCompletionValidationIsPrivate(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	for _, field := range []string{"query", "unknown"} {
		t.Run(field, func(t *testing.T) {
			sentinel := "private-completion-sentinel"
			input := map[string]any{"search": map[string]any{"query": "synthetic", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{}}}, "actual_mode": "lexical", "coverage": map[string]any{"state": "complete", "binding_required": false, "scoped_documents": 0, "complete_documents": 0}}
			if field == "query" {
				search, ok := input["search"].(map[string]any)
				require.True(t, ok)
				search["query"] = sentinel + strings.Repeat("x", 8193)
			} else {
				input["unknown"] = sentinel
			}
			resp, body := do(t, ts, http.MethodPost, "/api/v1/search/complete-empty", nil, input)
			require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
			require.NotContains(t, body, sentinel)
		})
	}
}

func TestMissLogDeletionStoreErrorsArePrivate(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, nil)
	require.NoError(t, st.Close())
	for _, path := range []string{"/api/v1/miss-log/synthetic-private-id", "/api/v1/miss-log?before=2100-01-01T00:00:00Z", "/api/v1/miss-log"} {
		resp, body := do(t, ts, http.MethodDelete, path, nil, nil)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
		require.Equal(t, "miss_log_unavailable", decodeProblem(t, body).Code)
		require.NotContains(t, body, "database is closed")
		require.NotContains(t, body, "synthetic-private-id")
	}
}
