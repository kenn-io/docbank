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

func TestVaultFindConflictsSegmentBoundary(t *testing.T) {
	t.Parallel()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
		"local": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider},
	}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	for i, qualifier := range []string{"premium", "standard"} {
		prefix := "For " + qualifier + " accounts, "
		text := prefix + strings.Repeat("x", 1000-len(prefix)-1) + " the cooling period is " + []string{"7", "15"}[i] + " days."
		receipt, err := vault.Put(t.Context(), "/"+qualifier+".txt", strings.NewReader(text), docbank.PutOptions{MediaType: "text/plain"})
		require.NoError(t, err)
		request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "local"}}
		plan, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
	}
	scope, err := uuid.Parse(vault.ID())
	require.NoError(t, err)
	result, err := vault.CallReadTool(t.Context(), scope, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
		Name: "find_conflicts", Arguments: []byte(`{"query":"cooling period","profile":"local","filters":{}}`),
	}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var out struct {
		Status   string `json:"status"`
		Passages []struct {
			Compared bool `json:"compared"`
		} `json:"passages"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Passages, 2)
	require.Equal(t, "NOT_COMPARABLE", out.Status)
	for _, passage := range out.Passages {
		require.False(t, passage.Compared)
	}
}

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
	for _, name := range []string{"get_vault_info", "list_documents", "search_documents", "find_conflicts"} {
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
	assert.Equal(t, vault.ID(), found["vault_id"])
	results, ok := found["results"].([]any)
	require.True(t, ok)
	require.Len(t, results, 1)
	resultItem, ok := results[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/alpha.txt", resultItem["path"])
	comparison := payload("find_conflicts", map[string]any{"query": "alpha", "profile": "local", "filters": map[string]any{}})
	assert.Equal(t, vault.ID(), comparison["vault_id"])
	assert.Equal(t, "NOT_COMPARABLE", comparison["status"])
	passages, ok := comparison["passages"].([]any)
	require.True(t, ok)
	require.Len(t, passages, 1)
	passage, ok := passages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/alpha.txt", passage["path"])
	assert.Equal(t, false, passage["compared"])
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
