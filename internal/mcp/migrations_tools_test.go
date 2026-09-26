package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPMigrationWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false, false))
	assert.NotContains(t, readOnly, "inventory_fotobank")
	assert.Contains(t, readOnly, "list_migration_runs")
	assert.Contains(t, readOnly, "show_migration_run")

	enabled := catalogMap(toolCatalog(false, false, false, true))
	inventory, ok := enabled["inventory_fotobank"]
	require.True(t, ok)
	require.NotNil(t, inventory.Annotations)
	assert.False(t, inventory.Annotations.ReadOnlyHint)
	assert.False(t, inventory.Annotations.IdempotentHint)
	assert.Equal(t, new(false), inventory.Annotations.DestructiveHint)
	assert.Equal(t, new(true), inventory.Annotations.OpenWorldHint)
	assertSchemaContract(t, inventory.InputSchema)
	assertSchemaContract(t, inventory.OutputSchema)

	readOnlyServer := newServerWithOptions(testImplementation(), ServerOptions{})
	readOnlyDiscovery := decodeResult(t, exchangeRaw(t, readOnlyServer, requestFor("server/discover", nil)))
	assert.NotContains(t, readOnlyDiscovery["instructions"], "Fotobank inventory")
	readOnlyList := decodeResult(t, exchangeRaw(t, readOnlyServer, requestFor("tools/list", nil)))
	assert.NotContains(t, listedToolNames(t, readOnlyList), "inventory_fotobank")

	enabledServer := newServerWithOptions(testImplementation(), ServerOptions{AllowMigrationWrites: true})
	discovery := decodeResult(t, exchangeRaw(t, enabledServer, requestFor("server/discover", nil)))
	instructions, ok := discovery["instructions"].(string)
	require.True(t, ok)
	assert.Contains(t, instructions, "Fotobank inventory")
	list := decodeResult(t, exchangeRaw(t, enabledServer, requestFor("tools/list", nil)))
	assert.Contains(t, listedToolNames(t, list), "inventory_fotobank")
}

func TestMigrationMCPUsesDaemon(t *testing.T) {
	ownerMapPath := t.TempDir() + "/owner-map.json"
	createdAt := "2026-09-25T12:00:00Z"
	run := api.MigrationRun{
		ID:     "00000000-0000-4000-8000-000000000001",
		Source: api.MigrationSource{Kind: "install", Identity: strings.Repeat("a", 64)}, CreatedAt: createdAt,
		Report: api.MigrationReport{
			Source: api.MigrationSource{Kind: "install", Identity: strings.Repeat("a", 64)},
			Counts: api.MigrationCounts{AlbumMemberships: 1, CheckoutEntries: 2}, Capacity: api.MigrationCapacity{}, CreatedAt: createdAt,
		},
		OwnerMap:     api.MigrationOwnerMap{Source: api.MigrationSource{Kind: "install", Identity: strings.Repeat("a", 64)}, Entries: []api.MigrationMapEntry{}},
		OwnerMapPath: ownerMapPath,
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/migrations/fotobank/inventories" {
			http.NotFound(response, request)
			return
		}
		calls++
		var body api.FotobankInventoryRequest
		if err := json.UnmarshalRead(request.Body, &body); err != nil || body.OwnerMapPath != ownerMapPath {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_ = json.MarshalWrite(response, run)
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	request, err := json.Marshal(api.FotobankInventoryRequest{CatalogPath: "/source/catalog.sqlite", VaultRoot: "/source/vault", OwnerMapPath: ownerMapPath})
	require.NoError(t, err)
	output, err := inventoryFotobank(t.Context(), lease, request)
	require.NoError(t, err)
	assert.Equal(t, run.ID, output.ID)
	assert.Equal(t, ownerMapPath, output.OwnerMapPath)
	assert.Equal(t, int64(1), output.Report.Counts.AlbumMemberships)
	assert.Equal(t, int64(2), output.Report.Counts.CheckoutEntries)
	assert.Equal(t, 0, output.Report.VectorGenerationCount)
	assert.Equal(t, 0, output.OwnerMap.EntryCount)
	assert.Equal(t, run.OwnerMap.Source.Kind, output.OwnerMap.Source.Kind)
	assert.Equal(t, int32(1), int32(calls))
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(encoded, &value))
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false, false, true))["inventory_fotobank"].OutputSchema, value)
}

func TestMCPMigrationRunSummariesStayBounded(t *testing.T) {
	const (
		entryCount  = 2_000
		vectorCount = 1_024
		listCount   = 50
	)
	ownerMapPath := t.TempDir() + "/owner-map.json"
	createdAt := "2026-09-25T12:00:00Z"
	identity := strings.Repeat("a", 64)
	entries := make([]api.MigrationMapEntry, entryCount)
	for index := range entries {
		entries[index] = api.MigrationMapEntry{
			SourceHub:      strings.Repeat("h", 256),
			SourceUserID:   strings.Repeat("u", 256),
			StorageKey:     strings.Repeat("k", 128),
			DocbankOwnerID: fmt.Sprintf("00000000-0000-4000-8000-%012d", index),
		}
	}
	vectors := make([]api.MigrationVectorGeneration, vectorCount)
	for index := range vectors {
		vectors[index] = api.MigrationVectorGeneration{
			ID:          int64(index + 1),
			Fingerprint: strings.Repeat("f", 256),
			State:       strings.Repeat("s", 64),
			Rebuildable: true,
		}
	}
	run := api.MigrationRun{
		ID:        "00000000-0000-4000-8000-000000000001",
		Source:    api.MigrationSource{Kind: "install", Identity: identity},
		CreatedAt: createdAt,
		Report: api.MigrationReport{
			Source:    api.MigrationSource{Kind: "install", Identity: identity},
			Schema:    api.MigrationSchema{CatalogVersion: 1, CatalogFingerprint: strings.Repeat("c", 128), EmbeddedDocbankVersion: 16},
			Counts:    api.MigrationCounts{Owners: entryCount, Assets: 2, Files: 3, Bytes: 4, Albums: 5, AlbumMemberships: 6, Shares: 7, Checkouts: 8, CheckoutEntries: 9, AIResults: 10, HiddenSetup: 11},
			Vectors:   vectors,
			Capacity:  api.MigrationCapacity{SourceBytes: 12, UniqueBlobBytes: 13, MinimumContentBytes: 14},
			CreatedAt: createdAt,
		},
		OwnerMap:     api.MigrationOwnerMap{Source: api.MigrationSource{Kind: "install", Identity: identity}, Entries: entries},
		OwnerMapPath: ownerMapPath,
	}
	pageItems := make([]api.MigrationRun, listCount)
	for index := range pageItems {
		pageItems[index] = run
		pageItems[index].ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", index+2)
		pageItems[index].OwnerMapPath = ""
	}
	var inventoryCalls, listCalls, showCalls int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/migrations/fotobank/inventories":
			inventoryCalls++
			response.WriteHeader(http.StatusCreated)
			_ = json.MarshalWrite(response, run)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/migrations/runs":
			listCalls++
			_ = json.MarshalWrite(response, api.MigrationRunPage{Total: listCount, Items: pageItems})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/migrations/runs/"+run.ID:
			showCalls++
			_ = json.MarshalWrite(response, run)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	args, err := json.Marshal(api.FotobankInventoryRequest{
		CatalogPath: "/source/catalog.sqlite", VaultRoot: "/source/vault", OwnerMapPath: ownerMapPath,
	})
	require.NoError(t, err)
	createValidator := mustResolveSchema(catalogMap(toolCatalog(false, false, false, true))["inventory_fotobank"].OutputSchema)
	created, err := migrationInventoryToolHandler(lease, createValidator, slog.Default())(
		t.Context(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Arguments: args}},
	)
	require.NoError(t, err)
	require.False(t, created.IsError)
	assertMigrationSummaryResult(t, created, run, true)
	assert.Equal(t, int32(1), int32(inventoryCalls))

	shown, err := invokeReadTool(t.Context(), lease, "show_migration_run", map[string]any{"run_id": run.ID})
	require.NoError(t, err)
	require.False(t, shown.IsError)
	assertMigrationSummaryResult(t, shown, run, false)

	listed, err := invokeReadTool(t.Context(), lease, "list_migration_runs", map[string]any{"limit": listCount})
	require.NoError(t, err)
	require.False(t, listed.IsError)
	listedValue := structuredMap(t, listed.StructuredContent)
	items, ok := listedValue["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, listCount)
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, itemMap, "owner_map_path")
		report, ok := itemMap["report"].(map[string]any)
		require.True(t, ok)
		counts, ok := report["counts"].(map[string]any)
		require.True(t, ok)
		ownerMap, ok := itemMap["owner_map"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, report, "vectors")
		assert.NotContains(t, ownerMap, "entries")
		assert.InDelta(t, float64(entryCount), counts["owners"], 0)
		assert.InDelta(t, float64(vectorCount), report["vector_generation_count"], 0)
		assert.InDelta(t, float64(entryCount), ownerMap["entry_count"], 0)
	}
	assert.Equal(t, int32(1), int32(listCalls))
	assert.Equal(t, int32(1), int32(showCalls))
}

func TestMCPMigrationRunListNormalizesNilItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/migrations/runs" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(response, api.MigrationRunPage{Total: 0}); err != nil {
			t.Errorf("write migration run page: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "list_migration_runs", map[string]any{"limit": 50})
	require.NoError(t, err)
	assert.Equal(t, []any{}, structuredMap(t, result.StructuredContent)["items"])
}

func assertMigrationSummaryResult(t *testing.T, result *sdkmcp.CallToolResult, run api.MigrationRun, includePath bool) {
	t.Helper()
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), maxToolResponseBytes)
	value := structuredMap(t, result.StructuredContent)
	assert.Equal(t, run.ID, value["id"])
	report, ok := value["report"].(map[string]any)
	require.True(t, ok)
	counts, ok := report["counts"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, float64(run.Report.Counts.Owners), counts["owners"], 0)
	assert.InDelta(t, float64(run.Report.Counts.Assets), counts["assets"], 0)
	assert.InDelta(t, float64(run.Report.Counts.Files), counts["files"], 0)
	assert.InDelta(t, float64(run.Report.Counts.Bytes), counts["bytes"], 0)
	assert.InDelta(t, float64(run.Report.Counts.Albums), counts["albums"], 0)
	assert.InDelta(t, float64(run.Report.Counts.AlbumMemberships), counts["album_memberships"], 0)
	assert.InDelta(t, float64(run.Report.Counts.Shares), counts["shares"], 0)
	assert.InDelta(t, float64(run.Report.Counts.Checkouts), counts["checkouts"], 0)
	assert.InDelta(t, float64(run.Report.Counts.CheckoutEntries), counts["checkout_entries"], 0)
	assert.InDelta(t, float64(run.Report.Counts.AIResults), counts["ai_results"], 0)
	assert.InDelta(t, float64(run.Report.Counts.HiddenSetup), counts["hidden_setup"], 0)
	capacity, ok := report["capacity"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, float64(run.Report.Capacity.SourceBytes), capacity["source_bytes"], 0)
	assert.InDelta(t, float64(run.Report.Capacity.UniqueBlobBytes), capacity["unique_blob_bytes"], 0)
	assert.InDelta(t, float64(run.Report.Capacity.MinimumContentBytes), capacity["minimum_content_bytes"], 0)
	assert.InDelta(t, float64(len(run.Report.Vectors)), report["vector_generation_count"], 0)
	ownerMap, ok := value["owner_map"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, float64(len(run.OwnerMap.Entries)), ownerMap["entry_count"], 0)
	assert.NotContains(t, report, "vectors")
	assert.NotContains(t, ownerMap, "entries")
	if includePath {
		assert.Equal(t, run.OwnerMapPath, value["owner_map_path"])
	} else {
		assert.NotContains(t, value, "owner_map_path")
	}
}
