package docbank_test

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document/plaintext"
	catalog "go.kenn.io/docbank/mcp"
)

func TestVaultReadTools(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	vault, err := docbank.New(ctx, docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"local": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider}}}})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, vault.Close()) })
	_, err = vault.Put(ctx, "/alpha.txt", strings.NewReader("observatory"), docbank.PutOptions{})
	require.NoError(t, err)
	_, err = vault.Put(ctx, "/beta.txt", strings.NewReader("second"), docbank.PutOptions{})
	require.NoError(t, err)
	scope, err := uuid.Parse(vault.ID())
	require.NoError(t, err)
	var mu sync.Mutex
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "embedded-vault", Version: "1"}, nil)
	for _, name := range []string{"get_vault_info", "list_documents", "search_documents"} {
		tool, err := catalog.ReadTool(name)
		require.NoError(t, err)
		server.AddTool(tool, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			mu.Lock()
			id := scope
			mu.Unlock()
			return vault.CallReadTool(ctx, id, req)
		})
	}
	ct, st := sdkmcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ss.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "vault-reader", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cs.Close()) })
	call := func(name string, args map[string]any) (*sdkmcp.CallToolResult, error) {
		t.Helper()
		return cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	}
	payload := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := call(name, args)
		require.NoError(t, err)
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		require.False(t, result.IsError, string(encoded))
		encoded, err = json.Marshal(result.StructuredContent)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(encoded, &out))
		return out
	}
	page := payload("list_documents", map[string]any{"page_size": 1})
	items, ok := page["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/alpha.txt", item["path"])
	assert.Equal(t, "private", page["cacheScope"])
	assert.EqualValues(t, 0, page["ttlMs"])
	next := payload("list_documents", map[string]any{"page_size": 1, "cursor": page["next_cursor"]})
	nextItems, ok := next["items"].([]any)
	require.True(t, ok)
	require.Len(t, nextItems, 1)
	nextItem, ok := nextItems[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/beta.txt", nextItem["path"])
	info := payload("get_vault_info", map[string]any{})
	assert.Equal(t, vault.ID(), info["vault_id"])
	assert.EqualValues(t, 2, info["live_files"])
	assert.EqualValues(t, 2, info["content_versions"])
	found := payload("search_documents", map[string]any{"query": "alpha", "mode": "lexical", "profile": "local", "filters": map[string]any{}})
	assert.EqualValues(t, 2, found["observed_scope_count"])
	results, ok := found["results"].([]any)
	require.True(t, ok)
	require.Len(t, results, 1)
	resultItem, ok := results[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/alpha.txt", resultItem["path"])
	// An admitted whole-vault binding cannot be replaced with zero or another vault.
	for _, id := range []uuid.UUID{uuid.Nil(), uuid.NewV4()} {
		mu.Lock()
		scope = id
		mu.Unlock()
		result, err := call("list_documents", map[string]any{"page_size": 1, "cursor": page["next_cursor"]})
		require.Error(t, err)
		assert.Nil(t, result)
	}
	mu.Lock()
	scope, err = uuid.Parse(vault.ID())
	mu.Unlock()
	require.NoError(t, err)
	// Inventory reflects version changes without changing the current file count.
	_, err = vault.Put(ctx, "/alpha.txt", strings.NewReader("updated"), docbank.PutOptions{})
	require.NoError(t, err)
	info = payload("get_vault_info", map[string]any{})
	assert.EqualValues(t, 2, info["live_files"])
	assert.EqualValues(t, 3, info["content_versions"])
	_, err = call("list_documents", map[string]any{"page_size": -1})
	require.Error(t, err)
	unsupportedArgs, err := json.Marshal(map[string]any{
		"node_id": nextItem["node_id"], "content_version_id": nextItem["content_version_id"],
	})
	require.NoError(t, err)
	result, err := vault.CallReadTool(ctx, scope, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
		Name: "get_document", Arguments: unsupportedArgs,
	}})
	var rpcError *jsonrpc.Error
	require.ErrorAs(t, err, &rpcError)
	assert.EqualValues(t, jsonrpc.CodeInvalidParams, rpcError.Code)
	assert.Nil(t, result)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = vault.CallReadTool(canceled, scope, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "list_documents", Arguments: []byte(`{}`)}})
	require.ErrorIs(t, err, context.Canceled)
}
