package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/store"
)

func TestMissLogFeedbackMCPWritesWithoutReadTools(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(st), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	cfg.MissLog.Enabled = true
	svc := misslog.New(st, cfg.MissLog, nil)
	daemon := api.NewServer(api.Deps{Store: st, Blobs: blobs, VaultRoot: root, Cfg: cfg, MissLog: svc})
	defer daemon.Close()
	httpServer := httptest.NewServer(daemon.Handler())
	defer httpServer.Close()
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) { return connection, nil }, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	ticket := svc.Observe(t.Context(), misslog.Receipt{Query: "synthetic private question", Hits: []misslog.Hit{{Rank: 1, NodeID: 2}}})
	arguments := map[string]any{"id": ticket.ID, "capability": ticket.Capability, "feedback": map[string]any{"verdict": "wrong"}}
	response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "record_search_feedback", "arguments": arguments}))
	result := decodeResult(t, response)
	require.NotEqual(t, true, result["isError"])
	require.Equal(t, map[string]any{"id": ticket.ID, "success": true}, objectField(t, result, "structuredContent"))
	require.NotContains(t, string(response), "synthetic private question")
	require.NotContains(t, string(response), ticket.Capability)
	row, err := st.MissLogRecordByID(t.Context(), ticket.ID)
	require.NoError(t, err)
	require.Contains(t, row.Receipt, `"verdict":"wrong"`)
	arguments["capability"] = "0000000000000000000000000000000000000000000000000000000000000000"
	response = exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "record_search_feedback", "arguments": arguments}))
	require.NotContains(t, string(response), "synthetic private question")
	require.NotContains(t, string(response), `"success":true`)
	for _, tool := range toolCatalog(ServerOptions{}) {
		require.NotContains(t, tool.Name, "miss_log")
		if tool.Name == "record_search_feedback" {
			require.False(t, tool.Annotations.ReadOnlyHint)
		}
	}
	raw, err := json.Marshal(arguments)
	require.NoError(t, err)
	_, err = CallDocumentReadTool(t.Context(), nil, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "record_search_feedback", Arguments: raw}})
	require.Error(t, err)
}
