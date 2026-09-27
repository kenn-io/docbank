package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionMCPRecipesAndResolvedMaskUseDaemon(t *testing.T) {
	const setID = "79000000-0000-4000-8000-0000000000a1"
	const memberID = "79000000-0000-4000-8000-0000000000a2"
	digest := strings.Repeat("a", 64)
	catalog, err := api.QualifiedProductionRecipes()
	require.NoError(t, err)
	resolved := store.ProductionResolvedMaskPage{SetID: setID, Revision: 1, ETag: 2, MemberID: memberID,
		Page:      redaction.Page{Number: 1, FrameSHA256: digest, Width: 100, Height: 200},
		MapSHA256: digest, RecipeSHA256: digest, ResolvedSHA256: digest, ReviewBinding: digest,
		TotalBoxes: 1, Items: []redaction.Box{{Page: 1, FrameSHA256: digest, X0: 1, Y0: 2, X1: 3, Y1: 4}}}
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		switch request.URL.Path {
		case "/api/v1/productions/recipes":
			assert.Equal(t, http.MethodGet, request.Method)
			writeDaemonJSON(t, response, catalog)
		case "/api/v1/productions/sets/" + setID + "/revisions/1/resolve":
			assert.Equal(t, http.MethodPost, request.Method)
			assert.Equal(t, "2", request.Header.Get("If-Match"))
			writeDaemonJSON(t, response, api.ProductionResolvedMaskPage(resolved))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, false)
	recipes := objectField(t, callToolResult(t, server, "list_production_recipes", map[string]any{}), "structuredContent")
	require.Len(t, recipes["items"], 2)
	assert.Equal(t, redaction.DefaultRecipeID, recipes["default_id"])
	assert.Equal(t, "private", recipes["cacheScope"])
	mask := objectField(t, callToolResult(t, server, "resolve_production_selection", map[string]any{
		"set_id": setID, "revision": 1, "etag": 2, "member_id": memberID, "page": 1, "limit": 1}), "structuredContent")
	assert.Equal(t, digest, mask["review_binding"])
	require.Len(t, mask["items"], 1)
	assert.Equal(t, "private", mask["cacheScope"])
	assert.Equal(t, 2, calls)
}

func TestProductionMCPResolveSchemasBoundInputs(t *testing.T) {
	tools := catalogMap(toolCatalog(false))
	recipes := tools["list_production_recipes"]
	require.NotNil(t, recipes)
	resolve := tools["resolve_production_selection"]
	require.NotNil(t, resolve)
	valid := map[string]any{"set_id": "79000000-0000-4000-8000-0000000000a1", "revision": 1,
		"etag": 2, "member_id": "79000000-0000-4000-8000-0000000000a2", "page": 1, "limit": 1}
	assertSchemaAccepts(t, resolve.InputSchema, valid)
	valid["limit"] = 201
	assertSchemaRejects(t, resolve.InputSchema, valid)
	valid["limit"] = 1
	valid["etag"] = 0
	assertSchemaRejects(t, resolve.InputSchema, valid)
}
