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

func TestProductionMCPSetCreateAndForkRequireWriteOptIn(t *testing.T) {
	const createOperation = "79000000-0000-4000-8000-0000000000b1"
	const forkOperation = "79000000-0000-4000-8000-0000000000b2"
	vault, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	set, draft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: createOperation, Name: "Synthetic MCP set"})
	require.NoError(t, err)
	forked, err := vault.ForkProductionDraft(t.Context(), "synthetic-operator", set.ID, 1, forkOperation)
	require.NoError(t, err)
	readOnly := newBatesToolTestServer(t, "http://127.0.0.1:1", false)
	missing := exchangeRaw(t, readOnly, requestFor("tools/call", map[string]any{
		"name": "create_production_set", "arguments": map[string]any{
			"operation_id": createOperation, "name": "Synthetic MCP set"},
	}))
	assert.NotZero(t, decodeWireError(t, missing).Code)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, request.Method)
		switch request.URL.Path {
		case "/api/v1/productions/sets":
			var input redaction.CreateRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &input)) {
				return
			}
			assert.Equal(t, createOperation, input.OperationID)
			if input.Name == "Changed synthetic MCP set" {
				response.Header().Set("Content-Type", "application/problem+json")
				response.WriteHeader(http.StatusConflict)
				writeDaemonJSON(t, response, api.NewError(http.StatusConflict,
					"production_operation_conflict", "synthetic private detail"))
				return
			}
			assert.Equal(t, set.Name, input.Name)
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			writeDaemonJSON(t, response, api.ProductionSetCreated{Set: set, Draft: draft})
		case "/api/v1/productions/sets/" + set.ID + "/revisions/1/fork":
			var input api.ProductionForkRequest
			if !assert.NoError(t, decodeDaemonJSON(request.Body, &input)) {
				return
			}
			assert.Equal(t, forkOperation, input.OperationID)
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			writeDaemonJSON(t, response, forked)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	created := objectField(t, callToolResult(t, server, "create_production_set", map[string]any{
		"operation_id": createOperation, "name": "Synthetic MCP set"}), "structuredContent")
	assert.Equal(t, set.ID, objectField(t, created, "set")["id"])
	assert.Equal(t, "private", created["cacheScope"])
	fork := objectField(t, callToolResult(t, server, "fork_production_draft", map[string]any{
		"set_id": set.ID, "revision": 1, "operation_id": forkOperation}), "structuredContent")
	assert.EqualValues(t, forked.Revision, objectField(t, fork, "draft")["revision"])
	conflict := callToolResult(t, server, "create_production_set", map[string]any{
		"operation_id": createOperation, "name": "Changed synthetic MCP set"})
	assert.Equal(t, true, conflict["isError"])
	assert.Equal(t, "production_operation_conflict",
		objectField(t, conflict, "structuredContent")["code"])
	assert.Equal(t, 3, calls)
}

func TestProductionMCPDraftWriteSchemasRequireOperationIdentity(t *testing.T) {
	tools := catalogMap(toolCatalog(true))
	create := tools["create_production_set"]
	require.NotNil(t, create)
	assertSchemaRejects(t, create.InputSchema, map[string]any{"name": "Synthetic MCP set"})
	assertSchemaRejects(t, create.InputSchema, map[string]any{
		"operation_id": "invalid", "name": "Synthetic MCP set"})
	fork := tools["fork_production_draft"]
	require.NotNil(t, fork)
	assertSchemaRejects(t, fork.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-0000000000b3", "revision": 1})
}

func TestProductionMCPCreateReportsUnknownOutcomeWithoutRetry(t *testing.T) {
	const operationID = "79000000-0000-4000-8000-0000000000b4"
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		calls++
		hijacker, ok := response.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		connection, _, err := hijacker.Hijack()
		if !assert.NoError(t, err) {
			return
		}
		assert.NoError(t, connection.Close())
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := callToolResult(t, server, "create_production_set", map[string]any{
		"operation_id": operationID, "name": "Synthetic lost response"})
	assert.Equal(t, true, result["isError"])
	assert.Equal(t, "production_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	assert.Equal(t, 1, calls)
}
