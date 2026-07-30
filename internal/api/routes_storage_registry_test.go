package api_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
)

func TestStorageRegistrationPreviewApplyAndRemoval(t *testing.T) {
	namespace := filepath.Join(t.TempDir(), "archive")
	ts, live := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"archive_nas": {Kind: "filesystem", Path: namespace, Priority: 25},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "archive", "binding": "archive_nas"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview api.BlobStorePreview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	assert.Equal(t, "create", preview.MarkerAction)
	assert.Equal(t, "archive", preview.Store.Name)
	assert.NotEmpty(t, preview.PreviewToken)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil,
		map[string]any{"preview_token": preview.PreviewToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var registered api.BlobStore
	require.NoError(t, json.Unmarshal([]byte(body), &registered))
	assert.Equal(t, "online", registered.State)
	assert.Equal(t, "archive_nas", registered.Binding)
	assert.DirExists(t, namespace)

	resp, body = get(t, ts, "/api/v1/storage/stores?refresh=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var stores []api.BlobStore
	require.NoError(t, json.Unmarshal([]byte(body), &stores))
	require.Len(t, stores, 2)
	assert.Equal(t, "primary", stores[0].Role)
	assert.Equal(t, registered.ID, stores[1].ID)

	resp, body = do(t, ts, http.MethodPost,
		"/api/v1/storage/stores/"+registered.ID+"/detach", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete,
		"/api/v1/storage/stores/"+registered.ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	_, err := live.BlobStoreBySelector(t.Context(), registered.ID)
	require.Error(t, err)
}

func TestStorageRegistrationRejectsVaultOverlap(t *testing.T) {
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"inside": {
				Kind: "filesystem", Path: filepath.Join(d.VaultRoot, "secondary"),
			},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})
	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "unsafe", "binding": "inside"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_namespace_overlap"`)
}

func TestStorageRegistrationRejectsCatalogChangeBeforeMarkerHandoff(t *testing.T) {
	namespace := filepath.Join(t.TempDir(), "archive")
	ts, live := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"archive_nas": {Kind: "filesystem", Path: namespace},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})
	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "archive", "binding": "archive_nas"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview api.BlobStorePreview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))

	conflict, err := live.PrepareSecondaryBlobStore("archive", "filesystem", "other")
	require.NoError(t, err)
	require.NoError(t, live.RegisterBlobStore(t.Context(), conflict))

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil,
		map[string]any{"preview_token": preview.PreviewToken})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_preview_stale"`)
	assert.NoDirExists(t, namespace)
}
