package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func newProductionPolicyToolDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(catalog), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	transport := httptest.NewServer(server.Handler())
	t.Cleanup(transport.Close)
	return transport
}

func policyToolArguments(t *testing.T, version int64) map[string]any {
	t.Helper()
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Version: version,
		Name: "Synthetic policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}}, ConflictMode: documentproduction.PolicyConflictReject,
	}
	encoded, err := json.Marshal(policy)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(encoded, &value))
	return value
}

func TestProductionPolicyToolsCompleteWorkflowThroughRealDaemon(t *testing.T) {
	daemon := newProductionPolicyToolDaemon(t)
	readOnly := newBatesToolTestServer(t, daemon.URL, false)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_policy"])
	require.NotNil(t, catalogMap(toolCatalog(false))["list_production_policies"])
	require.NotNil(t, catalogMap(toolCatalog(false))["get_production_policy"])
	missing := exchangeRaw(t, readOnly, requestFor("tools/call", map[string]any{
		"name": "create_production_policy", "arguments": map[string]any{
			"operation_id": "22222222-2222-4222-8222-222222222222", "policy": policyToolArguments(t, 1)},
	}))
	require.NotZero(t, decodeWireError(t, missing).Code)
	writes := newBatesToolTestServer(t, daemon.URL, true)
	const operationID = "22222222-2222-4222-8222-222222222222"
	firstPolicy := policyToolArguments(t, 1)
	created := callToolResult(t, writes, "create_production_policy", map[string]any{
		"operation_id": operationID, "policy": firstPolicy})
	first := objectField(t, objectField(t, created, "structuredContent"), "policy")
	require.Equal(t, firstPolicy["id"], first["id"])
	require.NotEmpty(t, first["sha256"])
	replayed := callToolResult(t, writes, "create_production_policy", map[string]any{
		"operation_id": operationID, "policy": firstPolicy})
	require.Equal(t, first["sha256"], objectField(t, objectField(t, replayed, "structuredContent"), "policy")["sha256"])

	secondPolicy := policyToolArguments(t, 2)
	second := callToolResult(t, writes, "create_production_policy", map[string]any{
		"operation_id": "33333333-3333-4333-8333-333333333333", "policy": secondPolicy})
	require.NotEqual(t, first["sha256"], objectField(t, objectField(t, second, "structuredContent"), "policy")["sha256"])

	listed := callToolResult(t, readOnly, "list_production_policies", map[string]any{"limit": 1})
	page := objectField(t, listed, "structuredContent")
	require.Len(t, page["items"], 1)
	require.NotEmpty(t, page["next_cursor"])
	next := callToolResult(t, readOnly, "list_production_policies", map[string]any{
		"limit": 1, "cursor": page["next_cursor"]})
	nextPage := objectField(t, next, "structuredContent")
	require.Len(t, nextPage["items"], 1)
	require.Empty(t, nextPage["next_cursor"])

	shown := callToolResult(t, readOnly, "get_production_policy", map[string]any{
		"policy_id": firstPolicy["id"], "version": 1})
	require.Equal(t, first["sha256"], objectField(t, objectField(t, shown, "structuredContent"), "policy")["sha256"])

	changedPolicy := policyToolArguments(t, 1)
	changedPolicy["name"] = "Altered synthetic policy"
	conflict := callToolResult(t, writes, "create_production_policy", map[string]any{
		"operation_id": operationID, "policy": changedPolicy})
	require.Equal(t, true, conflict["isError"])
	require.Equal(t, "production_policy_conflict", objectField(t, conflict, "structuredContent")["code"])
}

func TestProductionPolicyToolSchemasEnforceBounds(t *testing.T) {
	tools := catalogMap(toolCatalog(true))
	create := tools["create_production_policy"]
	require.NotNil(t, create)
	args := map[string]any{"operation_id": "22222222-2222-4222-8222-222222222222",
		"policy": policyToolArguments(t, 1)}
	assertSchemaAccepts(t, create.InputSchema, args)
	policy := policyToolArguments(t, 1)
	policy["sha256"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	assertSchemaRejects(t, create.InputSchema, map[string]any{"operation_id": args["operation_id"], "policy": policy})
	assertSchemaRejects(t, create.InputSchema, map[string]any{"policy": policyToolArguments(t, 1)})
	list := tools["list_production_policies"]
	require.NotNil(t, list)
	assertSchemaAccepts(t, list.InputSchema, map[string]any{"limit": 100})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"limit": 101})
	assertSchemaRejects(t, list.InputSchema, map[string]any{"cursor": strings.Repeat("x", 61)})
}

func TestProductionPolicyToolReportsUnknownWriteOutcomeWithoutRetry(t *testing.T) {
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := callToolResult(t, server, "create_production_policy", map[string]any{
		"operation_id": "22222222-2222-4222-8222-222222222222", "policy": policyToolArguments(t, 1)})
	require.Equal(t, true, result["isError"])
	require.Equal(t, "production_policy_outcome_unknown", objectField(t, result, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
