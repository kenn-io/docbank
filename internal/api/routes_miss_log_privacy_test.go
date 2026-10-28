package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestMissLogSubmittedQueryValidationIsPrivate(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	sentinel := "private-miss-sentinel"
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil,
		map[string]any{"query": sentinel + strings.Repeat("x", 8192)})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.NotContains(t, body, sentinel)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMissLogDeletionStoreErrorsArePrivate(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, nil)
	require.NoError(t, st.Close())
	for _, path := range []string{"/api/v1/miss-log/synthetic-private-id", "/api/v1/miss-log"} {
		resp, body := do(t, ts, http.MethodDelete, path, nil, nil)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
		require.Equal(t, "miss_log_unavailable", decodeProblem(t, body).Code)
		require.NotContains(t, body, "database is closed")
		require.NotContains(t, body, "synthetic-private-id")
	}
}
