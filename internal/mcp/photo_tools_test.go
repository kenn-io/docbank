package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotoMCPWorkflowAndWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false))
	assert.Contains(t, readOnly, "get_photo_asset")
	for _, name := range []string{
		"create_photo_asset", "attach_photo_file", "detach_photo_file",
		"exclude_photo_asset", "promote_photo_asset",
	} {
		assert.NotContains(t, readOnly, name)
	}

	withWrites := catalogMap(toolCatalog(false, false, true))
	for _, name := range []string{
		"create_photo_asset", "attach_photo_file", "detach_photo_file",
		"exclude_photo_asset", "promote_photo_asset",
	} {
		tool := withWrites[name]
		require.NotNil(t, tool)
		require.NotNil(t, tool.Annotations)
		assert.False(t, tool.Annotations.ReadOnlyHint)
		assertSchemaContract(t, tool.InputSchema)
		assertSchemaContract(t, tool.OutputSchema)
	}
	assert.NotContains(t, withWrites, "set_photo_display")
	assert.NotContains(t, withWrites, "set_photo_settings")

	server := newServerWithOptions(testImplementation(), ServerOptions{AllowPhotoEdits: true})
	discovery := decodeResult(t, exchangeRaw(t, server, requestFor("server/discover", nil)))
	assert.Equal(t, catalogInstructions(false, false, true), discovery["instructions"])
}

func TestPhotoWriteNoReplay(t *testing.T) {
	var requests atomic.Int32
	disconnected := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			return
		}
		connection, _, err := hijacker.Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	t.Cleanup(disconnected.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(disconnected.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, false, true))["create_photo_asset"].OutputSchema)

	_, err := executePhotoWriteTool(t.Context(), lease, "create_photo_asset", validator,
		[]byte(`{"node_id":7}`))
	require.Error(t, err)
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
	assert.Equal(t, int32(1), requests.Load())
}

func TestPhotoWriteTreatsMalformedSuccessAsUnknown(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	for _, test := range []struct {
		name     string
		etag     string
		revision int64
		kind     string
		status   int
	}{
		{name: "missing ETag", revision: 1, kind: "photo"},
		{name: "semantic revision", etag: `"2"`, revision: 2, kind: "photo"},
		{name: "invalid asset", etag: `"1"`, revision: 1, kind: "document"},
		{name: "unexpected no-content success", revision: 1, kind: "photo", status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			asset := api.PhotoAsset{
				ID: assetID, Kind: test.kind, Revision: test.revision,
				DisplayFileID: &fileID, DisplaySource: "default",
				CreatedAt: createdAt, UpdatedAt: createdAt,
				Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 7, Role: "raw", CreatedAt: createdAt}},
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if test.etag != "" {
					w.Header().Set("ETag", test.etag)
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.MarshalWrite(w, asset))
			}))
			t.Cleanup(server.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(server.URL, "synthetic-key"), nil
			}, func(*daemonconn.Connection) error { return nil })
			validator := mustResolveSchema(catalogMap(toolCatalog(false, true))["create_photo_asset"].OutputSchema)

			_, err := executePhotoWriteTool(t.Context(), lease, "create_photo_asset", validator, []byte(`{"node_id":7}`))
			require.ErrorIs(t, err, errProcessingOutcomeUnknown)
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestPhotoWriteTreatsOutputSchemaFailureAsUnknown(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	invalidTime := "not-a-date-time"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 2, ExcludedAt: &invalidTime,
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 7, Role: "image", CreatedAt: createdAt}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		assert.NoError(t, json.MarshalWrite(w, asset))
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, true))["exclude_photo_asset"].OutputSchema)

	_, err := executePhotoWriteTool(t.Context(), lease, "exclude_photo_asset", validator,
		[]byte(`{"asset_id":"`+assetID+`","revision":1,"excluded":true}`))
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
}

func TestPhotoMCPCreateUsesDaemonRouteAndReturnsTimestamps(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 1,
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 7, Role: "raw", CreatedAt: createdAt}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/photos/assets" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		w.WriteHeader(http.StatusCreated)
		if err := json.MarshalWrite(w, asset); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, true))["create_photo_asset"].OutputSchema)

	result, err := executePhotoWriteTool(t.Context(), lease, "create_photo_asset", validator,
		[]byte(`{"node_id":7,"role":"raw"}`))
	require.NoError(t, err)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, createdAt, structured["created_at"])
	assert.Equal(t, createdAt, structured["updated_at"])
}
