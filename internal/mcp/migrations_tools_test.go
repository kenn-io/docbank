package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPMigrationWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false))
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
			Counts: api.MigrationCounts{}, Capacity: api.MigrationCapacity{}, CreatedAt: createdAt,
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
	assert.Equal(t, int32(1), int32(calls))
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(encoded, &value))
	assertSchemaAccepts(t, catalogMap(toolCatalog(false, false, false, true))["inventory_fotobank"].OutputSchema, value)
}
