package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
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
	inspector, err := blob.NewFilesystemBackend(namespace, nil)
	require.NoError(t, err)
	current, err := inspector.Ownership(t.Context())
	require.NoError(t, err)
	taken := current
	taken.Epoch = "50000000-0000-4000-8000-000000000011"
	require.NoError(t, inspector.ReplaceOwnership(t.Context(), taken, &current))
	require.NoError(t, inspector.Close())

	resp, body = get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Len(t, status.Stores, 2)
	assert.Equal(t, "online", status.Stores[1].State)
	resp, body = get(t, ts, "/api/v1/storage?refresh=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Len(t, status.Stores, 2)
	assert.Equal(t, "fenced", status.Stores[1].State)

	resp, body = do(t, ts, http.MethodPost,
		"/api/v1/storage/stores/"+registered.ID+"/detach", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete,
		"/api/v1/storage/stores/"+registered.ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	_, err = live.BlobStoreBySelector(t.Context(), registered.ID)
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

func TestStorageRegistrationRejectsUnmarkedNonemptyNamespace(t *testing.T) {
	namespace := t.TempDir()
	require.NoError(t, blob.EnsureFilesystemNamespace(namespace))
	require.NoError(t, os.WriteFile(
		filepath.Join(namespace, "operator-note.txt"), []byte("preserve"), 0o600,
	))
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"occupied": {Kind: "filesystem", Path: namespace},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "archive", "binding": "occupied"})

	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_namespace_not_empty"`)
}

func TestStorageRegistrationRejectsOverlappingSecondaries(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "archive")
	inner := filepath.Join(outer, "nested")
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"outer": {Kind: "filesystem", Path: outer},
			"inner": {Kind: "filesystem", Path: inner},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})
	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "outer", "binding": "outer"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview api.BlobStorePreview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil,
		map[string]any{"preview_token": preview.PreviewToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "inner", "binding": "inner"})

	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_namespace_overlap"`)
}

func TestStorageRegistrationRejectsOverlappingS3Prefixes(t *testing.T) {
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"outer": {
				Kind: "s3", Endpoint: "https://objects.example", Region: "us-east-1",
				Bucket: "archive", Prefix: "docbank", CredentialProfile: "test",
			},
			"inner": {
				Kind: "s3", Endpoint: "https://OBJECTS.EXAMPLE/", Region: "us-east-1",
				Bucket: "archive", Prefix: "docbank/nested", CredentialProfile: "test",
			},
		}
		existing, err := d.Store.PrepareSecondaryBlobStore("outer", "s3", "outer")
		require.NoError(t, err)
		require.NoError(t, d.Store.RegisterBlobStore(t.Context(), existing))
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings,
			[]blob.StoreSpec{{
				ID: existing.ID, Kind: existing.Kind, Role: existing.Role,
				Lifecycle: existing.Lifecycle, Binding: existing.Binding,
				OwnershipEpoch: existing.OwnershipEpoch,
			}},
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "inner", "binding": "inner"})

	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_namespace_overlap"`)
}

func TestStorageRegistrationRejectsInvalidFirstS3Namespace(t *testing.T) {
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"invalid": {
				Kind: "s3", Region: "us-east-1", Bucket: "archive",
				Prefix: "../other-vault", CredentialProfile: "test",
			},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "archive", "binding": "invalid"})

	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_binding_invalid"`)
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

func TestStorageEvacuationPreviewRunsAndDetachesEmptySecondary(t *testing.T) {
	namespace := filepath.Join(t.TempDir(), "archive")
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Jobs = jobs.New(t.Context(), nil)
		t.Cleanup(d.Jobs.Stop)
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
	var registration api.BlobStorePreview
	require.NoError(t, json.Unmarshal([]byte(body), &registration))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil,
		map[string]any{"preview_token": registration.PreviewToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var registered api.BlobStore
	require.NoError(t, json.Unmarshal([]byte(body), &registered))

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/evacuate/preview", nil,
		map[string]any{"store": registered.ID})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview api.StoragePlacementPreview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	assert.Zero(t, preview.Objects)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/evacuate", nil,
		map[string]any{"preview_token": preview.PreviewToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var operation api.StorageOperation
	require.NoError(t, json.Unmarshal([]byte(body), &operation))

	require.Eventually(t, func() bool {
		resp, body = get(t, ts, "/api/v1/jobs/"+operation.ID, nil)
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var current api.StorageOperation
		return json.Unmarshal([]byte(body), &current) == nil &&
			current.State == "completed"
	}, 3*time.Second, 10*time.Millisecond)

	resp, body = get(t, ts, "/api/v1/storage/stores", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var stores []api.BlobStore
	require.NoError(t, json.Unmarshal([]byte(body), &stores))
	require.Len(t, stores, 2)
	assert.Equal(t, "detached", stores[1].Lifecycle)

	resp, body = do(t, ts, http.MethodDelete,
		"/api/v1/storage/stores/"+registered.ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
}
