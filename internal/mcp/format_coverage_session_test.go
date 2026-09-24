package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestFormatCoverageToolUsesScopedSessionAndStopsAfterRevocation(t *testing.T) {
	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-format-session-key"
	daemon := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
	t.Cleanup(daemon.Close)
	httpServer := httptest.NewServer(daemon.Handler())
	t.Cleanup(httpServer.Close)

	master := func(method, path string, body []byte) *http.Response {
		t.Helper()
		request, requestErr := http.NewRequestWithContext(t.Context(), method, httpServer.URL+path, bytes.NewReader(body))
		require.NoError(t, requestErr)
		request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		request.Header.Set("Content-Type", "application/json")
		response, requestErr := httpServer.Client().Do(request)
		require.NoError(t, requestErr)
		return response
	}
	issued := master(http.MethodPost, "/api/v1/agent-sessions", []byte(`{"operations":["read"],"source_ids":["synthetic-source"],"ttl_seconds":60}`))
	require.Equal(t, http.StatusCreated, issued.StatusCode)
	var session struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	require.NoError(t, json.UnmarshalRead(issued.Body, &session))
	require.NoError(t, issued.Body.Close())
	require.NotEmpty(t, session.Token)

	connection, err := daemonconn.AttachAgentSession(httpServer.URL, session.Token)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return connection, nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "get_format_coverage", map[string]any{"format": "pdf"})
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := structuredMap(t, result.StructuredContent)
	require.Equal(t, "format-coverage/v1", output["contract_version"])
	require.Len(t, output["formats"], 1)

	revoked := master(http.MethodDelete, "/api/v1/agent-sessions/"+session.ID, nil)
	require.Equal(t, http.StatusNoContent, revoked.StatusCode)
	require.NoError(t, revoked.Body.Close())
	result, err = invokeReadTool(t.Context(), lease, "get_format_coverage", map[string]any{"format": "pdf"})
	if err == nil {
		require.True(t, result.IsError)
	}
}
