package mcp

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionSetMCPReadsExactBoundedDaemonAuthority(t *testing.T) {
	vault, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	set, draft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "79000000-0000-4000-8000-000000000081", Name: "Synthetic MCP set"})
	require.NoError(t, err)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, request.Method)
		switch request.URL.Path {
		case "/api/v1/productions/sets":
			assert.Equal(t, "1", request.URL.Query().Get("limit"))
			writeDaemonJSON(t, response, api.ProductionSetPage{Items: []redaction.Set{set}})
		case "/api/v1/productions/sets/" + set.ID:
			writeDaemonJSON(t, response, set)
		case "/api/v1/productions/sets/" + set.ID + "/revisions/1":
			writeDaemonJSON(t, response, draft)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, false)
	listed := objectField(t, callToolResult(t, server, "list_production_sets",
		map[string]any{"limit": 1}), "structuredContent")
	require.Len(t, listed["items"], 1)
	assert.Equal(t, "private", listed["cacheScope"])
	shown := objectField(t, callToolResult(t, server, "get_production_set",
		map[string]any{"set_id": set.ID}), "structuredContent")
	assert.Equal(t, set.Name, objectField(t, shown, "set")["name"])
	view := objectField(t, callToolResult(t, server, "get_production_draft",
		map[string]any{"set_id": set.ID, "revision": 1}), "structuredContent")
	assert.Equal(t, draft.MemberHash, objectField(t, view, "draft")["member_hash"])
	assert.EqualValues(t, draft.ETag, objectField(t, view, "draft")["etag"])
	assert.Equal(t, 3, calls)
}

func TestProductionSetMCPReadSchemasBoundPagesAndIdentities(t *testing.T) {
	tools := catalogMap(toolCatalog(false))
	list := tools["list_production_sets"]
	require.NotNil(t, list)
	assertSchemaAccepts(t, list.InputSchema, map[string]any{"limit": 200})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"limit": 201})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"cursor": "x", "limit": 0})
	get := tools["get_production_set"]
	require.NotNil(t, get)
	assertSchemaRejects(t, get.InputSchema, map[string]any{"set_id": "bad"})
	draft := tools["get_production_draft"]
	require.NotNil(t, draft)
	assertSchemaRejects(t, draft.InputSchema, map[string]any{"set_id": "bad", "revision": 1})
	assertSchemaRejects(t, draft.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-000000000081", "revision": 0})
}
