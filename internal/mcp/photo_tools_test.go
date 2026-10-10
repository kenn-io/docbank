package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoMCPWorkflowAndWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(ServerOptions{}))
	assert.Contains(t, readOnly, "get_photo_asset")
	for _, name := range []string{
		"create_photo_asset", "attach_photo_file", "detach_photo_file",
		"exclude_photo_asset", "promote_photo_asset",
	} {
		assert.NotContains(t, readOnly, name)
	}

	withWrites := catalogMap(toolCatalog(ServerOptions{AllowPhotoEdits: true}))
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
	assert.Equal(t, catalogInstructions(ServerOptions{AllowPhotoEdits: true}),
		discovery["instructions"])
}

func TestPhotoMCPAuthoredPagesFitEnvelope(t *testing.T) {
	for _, test := range []struct {
		name  string
		text  string
		files int
	}{{"plain", "x", 24}, {"escaped", "\x01", 2}} {
		t.Run(test.name, func(t *testing.T) {
			stamp := "2026-09-22T00:00:00Z"
			asset := api.PhotoAsset{ID: "00000000-0000-4000-8000-000000000001", Kind: "photo", Revision: 2, DisplaySource: "default", CreatedAt: stamp, UpdatedAt: stamp, ExcludedAt: new(stamp)}
			text := strings.Repeat(test.text, store.MaxPhotoAuthoredTextBytes)
			for i := range test.files {
				asset.Files = append(asset.Files, api.PhotoFile{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+10), AssetID: asset.ID, NodeID: int64(i + 7), Role: "raw", Revision: 1, Caption: text, Creator: text, Copyright: text, CreatedAt: stamp})
			}
			asset.DisplayFileID = new(asset.Files[0].ID)
			var writes atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", `"2"`)
				if r.Method != http.MethodGet {
					writes.Add(1)
					if r.URL.Path == "/api/v1/photos/assets" {
						w.WriteHeader(http.StatusCreated)
					}
				}
				assert.NoError(t, json.MarshalWrite(w, asset))
			}))
			t.Cleanup(daemon.Close)
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return daemonconn.New(daemon.URL, "synthetic-key"), nil
			}, func(*daemonconn.Connection) error { return nil })
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPhotoEdits: true}, lease)
			var files []api.PhotoFile
			for offset := 0; ; {
				wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_photo_asset", "arguments": map[string]any{"asset_id": asset.ID, "file_offset": offset}}))
				require.Less(t, len(wire), maxToolResponseBytes)
				result := decodeResult(t, wire)
				encoded, err := json.Marshal(result["structuredContent"])
				require.NoError(t, err)
				var page photoAssetToolOutput
				require.NoError(t, json.Unmarshal(encoded, &page))
				require.Equal(t, len(asset.Files), page.TotalFiles)
				require.Equal(t, offset, page.FileOffset)
				require.NotEmpty(t, page.Files)
				files = append(files, page.Files...)
				if page.NextFileOffset == nil {
					break
				}
				require.Equal(t, offset+len(page.Files), *page.NextFileOffset)
				offset = *page.NextFileOffset
			}
			require.Equal(t, asset.Files, files)
			if test.name == "plain" {
				last, err := getPhotoAsset(t.Context(), lease, []byte(fmt.Sprintf(`{"asset_id":%q,"file_offset":%d}`, asset.ID, len(asset.Files))))
				require.NoError(t, err)
				assert.Empty(t, last.Files)
				assert.Nil(t, last.NextFileOffset)
				for _, offset := range []int{-1, 257} {
					wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_photo_asset", "arguments": map[string]any{"asset_id": asset.ID, "file_offset": offset}}))
					assert.EqualValues(t, -32602, decodeWireError(t, wire).Code)
				}
			}
			for _, mutation := range []struct {
				name string
				args map[string]any
			}{
				{"create_photo_asset", map[string]any{"node_id": 7}},
				{"attach_photo_file", map[string]any{"asset_id": asset.ID, "revision": 1, "node_id": 7, "role": "raw"}},
				{"detach_photo_file", map[string]any{"asset_id": asset.ID, "revision": 1, "file_id": "00000000-0000-4000-8000-000000000099"}},
				{"exclude_photo_asset", map[string]any{"asset_id": asset.ID, "revision": 1, "excluded": true}},
				{"promote_photo_asset", map[string]any{"node_id": 7}},
			} {
				before := writes.Load()
				wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": mutation.name, "arguments": mutation.args}))
				require.Less(t, len(wire), maxToolResponseBytes)
				result := decodeResult(t, wire)
				assert.NotEqual(t, true, result["isError"], mutation.name)
				output := objectField(t, result, "structuredContent")
				assert.EqualValues(t, 2, output["revision"], mutation.name)
				assert.EqualValues(t, test.files, output["total_files"], mutation.name)
				assert.Contains(t, output, "next_file_offset", mutation.name)
				assert.Equal(t, before+1, writes.Load(), mutation.name)
			}
		})
	}
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
	catalog := catalogMap(toolCatalog(ServerOptions{AllowPhotoEdits: true}))
	validator := mustResolveSchema(catalog["create_photo_asset"].OutputSchema)

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
		{name: "invalid asset", etag: `"1"`, revision: 1, kind: "document"},
		{name: "unexpected no-content success", revision: 1, kind: "photo", status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			asset := api.PhotoAsset{
				ID: assetID, Kind: test.kind, Revision: test.revision,
				DisplayFileID: &fileID, DisplaySource: "default",
				CreatedAt: createdAt, UpdatedAt: createdAt,
				Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 7, Role: "raw", Revision: 1, CreatedAt: createdAt}},
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
			catalog := catalogMap(toolCatalog(ServerOptions{AllowPhotoEdits: true}))
			validator := mustResolveSchema(catalog["create_photo_asset"].OutputSchema)

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
	catalog := catalogMap(toolCatalog(ServerOptions{AllowPhotoEdits: true}))
	validator := mustResolveSchema(catalog["exclude_photo_asset"].OutputSchema)

	_, err := executePhotoWriteTool(t.Context(), lease, "exclude_photo_asset", validator,
		[]byte(`{"asset_id":"`+assetID+`","revision":1,"excluded":true}`))
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
}

func TestPhotoMCPCreateUsesDaemonRouteAndReturnsTimestamps(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 1, Agreement: map[string]bool{"rating": false, "flag": true},
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 7, Role: "raw", Revision: 1, CreatedAt: createdAt}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodPost || r.URL.Path != "/api/v1/photos/assets") && (r.Method != http.MethodGet || r.URL.Path != "/api/v1/photos/assets/"+assetID) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		if err := json.MarshalWrite(w, asset); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	catalog := catalogMap(toolCatalog(ServerOptions{AllowPhotoEdits: true}))
	validator := mustResolveSchema(catalog["create_photo_asset"].OutputSchema)

	result, err := executePhotoWriteTool(t.Context(), lease, "create_photo_asset", validator,
		[]byte(`{"node_id":7,"role":"raw"}`))
	require.NoError(t, err)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, createdAt, structured["created_at"])
	assert.Equal(t, createdAt, structured["updated_at"])
	assert.Equal(t, map[string]any{"rating": false, "flag": true}, structured["agreement"])
	inspection, err := getPhotoAsset(t.Context(), lease, []byte(`{"asset_id":"`+assetID+`"}`))
	require.NoError(t, err)
	assert.Equal(t, asset.Agreement, inspection.Agreement)
}
