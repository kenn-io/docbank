package mcp

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func changesCall(t *testing.T, server *Server, transport, cursor string) changesOutput {
	t.Helper()
	arguments := map[string]any{}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
		"name": "list_changes", "arguments": arguments,
	}))
	require.NotEqual(t, true, result["isError"])
	data, err := json.Marshal(objectField(t, result, "structuredContent"))
	require.NoError(t, err)
	var output changesOutput
	require.NoError(t, json.Unmarshal(data, &output))
	return output
}

func TestMCPChangesResume(t *testing.T) {
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			fixture := newNativeExportFixture(t)
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, fixture.lease)
			fixture.addFile(t, "old.txt", "old content")
			initial := changesCall(t, server, transport, "")
			require.Empty(t, initial.Events)
			require.False(t, initial.HasMore)
			require.NotEmpty(t, initial.NextCursor)
			explicit := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
				"name": "list_changes", "arguments": map[string]any{"cursor": ""},
			}))
			explicitOutput := objectField(t, explicit, "structuredContent")
			require.Empty(t, explicitOutput["events"])
			require.Equal(t, initial.NextCursor, explicitOutput["next_cursor"])
			member := fixture.addFile(t, "first.txt", "first content")
			first := changesCall(t, server, transport, initial.NextCursor)
			require.Len(t, first.Events, 1)
			require.Equal(t, member.NodeID, first.Events[0].NodeID)
			require.False(t, first.HasMore)
			require.Equal(t, "private", first.CacheScope)
			require.Zero(t, first.TTLMs)
			require.Equal(t, first.Events[0].Cursor, first.NextCursor)
			empty := changesCall(t, server, transport, first.NextCursor)
			require.Empty(t, empty.Events)
			require.Equal(t, first.NextCursor, empty.NextCursor)
			later := fixture.addFile(t, "later.txt", "later content")
			next := changesCall(t, server, transport, empty.NextCursor)
			require.Len(t, next.Events, 1)
			require.Equal(t, later.NodeID, next.Events[0].NodeID)
		})
	}
}

func TestMCPChangesPageBoundary(t *testing.T) {
	fixture := newNativeExportFixture(t)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-changes-key"
	root := os.Getenv("DOCBANK_HOME")
	daemon := api.NewServer(api.Deps{Store: fixture.catalog, Blobs: fixture.blobs,
		VaultRoot: root, Cfg: cfg, ShutdownToken: "synthetic-proof"})
	t.Cleanup(daemon.Close)
	httpServer := httptest.NewServer(daemon.Handler())
	t.Cleanup(httpServer.Close)
	record := daemonconn.NewRecord(strings.TrimPrefix(httpServer.URL, "http://"), cfg.Server.APIKey, "synthetic-proof", "")
	_, err := daemonconn.RuntimeStore(root).Write(record)
	require.NoError(t, err)
	connection, running, err := daemonconn.Running(t.Context())
	require.NoError(t, err)
	require.True(t, running)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	server := NewServerWithConnection(connection, ServerOptions{})
	cursor := changesCall(t, server, "stdio", "").NextCursor
	var expected, received []int64
	for i := range 4_000 {
		node, err := fixture.catalog.Mkdir(t.Context(), fixture.catalog.RootID(), fmt.Sprintf("folder-%d", i))
		require.NoError(t, err)
		expected = append(expected, node.ID)
	}
	for pageIndex := range 8 {
		page := changesCall(t, server, "stdio", cursor)
		require.Len(t, page.Events, 500)
		require.Equal(t, pageIndex < 7, page.HasMore)
		require.Equal(t, page.Events[499].Cursor, page.NextCursor)
		cursor = page.NextCursor
		for _, event := range page.Events {
			received = append(received, event.NodeID)
		}
		vault := decodeResult(t, exportExchange(t, server, "stdio", "tools/call", map[string]any{
			"name": "get_vault_info", "arguments": map[string]any{},
		}))
		require.NotEqual(t, true, vault["isError"])
		require.Equal(t, fixture.catalog.VaultID(), objectField(t, vault, "structuredContent")["vault_id"])
	}
	require.Equal(t, expected, received)
}

func TestMCPChangesCursorErrors(t *testing.T) {
	fixture := newNativeExportFixture(t)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, fixture.lease)
	foreign := base64.RawURLEncoding.EncodeToString([]byte(uuid.New().String() + ":0"))
	result := decodeResult(t, exportExchange(t, server, "stdio", "tools/call", map[string]any{
		"name": "list_changes", "arguments": map[string]any{"cursor": foreign},
	}))
	require.Equal(t, true, result["isError"])
	output := objectField(t, result, "structuredContent")
	require.Equal(t, "changes_cursor_expired", output["code"])
	require.Contains(t, output["message"], "rescan")
	require.Contains(t, output["message"], "without a cursor")
}

func TestMCPChangesInterruptedDrain(t *testing.T) {
	saved := base64.RawURLEncoding.EncodeToString([]byte("00000000-0000-4000-8000-000000000001:0"))
	cursor := base64.RawURLEncoding.EncodeToString([]byte("00000000-0000-4000-8000-000000000001:1"))
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "false", r.URL.Query().Get("follow"))
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintf(w, `{"type":"change","change":{"cursor":%q,"kind":"created","node_id":7,"revision":1}}`+"\n", cursor)
	}))
	t.Cleanup(daemon.Close)
	result, err := invokeReadTool(t.Context(), exportTestLease(t, daemon.URL), "list_changes", map[string]any{"cursor": saved})
	require.ErrorIs(t, err, errDaemonRequestFailed)
	require.Nil(t, result)
	require.NotContains(t, err.Error(), cursor)
}
