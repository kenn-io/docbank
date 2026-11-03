package mcp

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
)

func changesCall(t *testing.T, server *Server, cursor string) changesOutput {
	t.Helper()
	arguments := map[string]any{}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	result := decodeResult(t, exportExchange(t, server, "stdio", "tools/call", map[string]any{
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
	fixture := newNativeExportFixture(t)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, fixture.lease)
	fixture.addFile(t, "old.txt", "old content")
	initial := changesCall(t, server, "")
	require.Empty(t, initial.Events)
	require.False(t, initial.HasMore)
	require.NotEmpty(t, initial.NextCursor)
	member := fixture.addFile(t, "first.txt", "first content")
	first := changesCall(t, server, initial.NextCursor)
	require.Len(t, first.Events, 1)
	require.Equal(t, member.NodeID, first.Events[0].NodeID)
	require.False(t, first.HasMore)
	require.Equal(t, "private", first.CacheScope)
	require.Zero(t, first.TTLMs)
	require.Equal(t, first.Events[0].Cursor, first.NextCursor)
}

func TestMCPChangesPageBoundary(t *testing.T) {
	fixture := newNativeExportFixture(t)
	server := NewServerWithConnection(fixture.provenConnection(t), ServerOptions{})
	cursor := changesCall(t, server, "").NextCursor
	var expected, received []int64
	for i := range 1_000 {
		node, err := fixture.catalog.Mkdir(t.Context(), fixture.catalog.RootID(), fmt.Sprintf("folder-%d", i))
		require.NoError(t, err)
		expected = append(expected, node.ID)
	}
	for pageIndex := range 2 {
		page := changesCall(t, server, cursor)
		require.NotEmpty(t, page.Events)
		require.Equal(t, pageIndex == 0, page.HasMore)
		require.Equal(t, page.Events[len(page.Events)-1].Cursor, page.NextCursor)
		cursor = page.NextCursor
		for _, event := range page.Events {
			received = append(received, event.NodeID)
		}
		if pageIndex == 0 {
			vault := decodeResult(t, exportExchange(t, server, "stdio", "tools/call", map[string]any{
				"name": "get_vault_info", "arguments": map[string]any{},
			}))
			require.NotEqual(t, true, vault["isError"])
			require.Equal(t, fixture.catalog.VaultID(), objectField(t, vault, "structuredContent")["vault_id"])
		}
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
