package main

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActionReportingEligibility(t *testing.T) {
	for _, tc := range []struct {
		name            string
		terminal, agent bool
		claude, codex   string
		want            bool
	}{
		{name: "terminal", terminal: true, want: true},
		{name: "pipe"},
		{name: "agent flag", terminal: true, agent: true},
		{name: "Claude", terminal: true, claude: "1"},
		{name: "Codex", terminal: true, codex: "synthetic-thread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDECODE", tc.claude)
			t.Setenv("CODEX_THREAD_ID", tc.codex)
			assert.Equal(t, tc.want, actionReportingAllowed(tc.terminal, tc.agent))
		})
	}
}

func TestTUIActionUsesDaemonAndExcludesAgents(t *testing.T) {
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/daemon/telemetry/events", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		events = append(events, string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"queued"}`)
	}))
	defer server.Close()
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}}
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	require.NoError(t, backend.ReportAction(t.Context(), "search_run", "lexical"))
	t.Setenv("CODEX_THREAD_ID", "synthetic-agent")
	require.NoError(t, backend.ReportAction(t.Context(), "search_run", "hybrid"))
	require.Len(t, events, 1)
	assert.JSONEq(t, `{"event":"search_run","properties":{"surface":"tui","search_type":"text"}}`, events[0])
}
