package api_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestStorageRegistrationPreviewApplyAndRemoval(t *testing.T) {
	t.Parallel()
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

func TestStorageRegistrationRejectsUnmarkedNonemptyNamespace(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestStorageRegistrationRejectsProtectedPathOverlaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		protectedPath string
		applyRechecks bool
	}{
		{name: "vault overlap", protectedPath: "vault"},
		{name: "watch overlap", protectedPath: "watch"},
		{name: "checkout overlap", protectedPath: "checkout"},
		{name: "apply rechecks reservation", protectedPath: "checkout", applyRechecks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := t.TempDir()
			watchRoot := t.TempDir()
			ts, live := newTestServer(t, func(d *api.Deps) {
				bindingPath := destination
				switch tc.protectedPath {
				case "vault":
					bindingPath = filepath.Join(d.VaultRoot, "secondary")
				case "watch":
					bindingPath = watchRoot
					d.Cfg.Watches = []config.WatchConfig{{Name: "inbox", Source: watchRoot, Destination: "/inbox"}}
				}
				d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{"overlap": {Kind: "filesystem", Path: bindingPath}}
				d.BlobRegistry = blob.NewRegistry(t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil)
				t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
			})
			endpoint := "/api/v1/storage/stores/preview"
			input := map[string]any{"name": "archive", "binding": "overlap"}
			if tc.applyRechecks {
				resp, body := do(t, ts, http.MethodPost, endpoint, nil, input)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				var preview api.BlobStorePreview
				require.NoError(t, json.Unmarshal([]byte(body), &preview))
				endpoint = "/api/v1/storage/stores"
				input = map[string]any{"preview_token": preview.PreviewToken}
			}
			var checkoutID string
			if tc.protectedPath == "checkout" {
				node, err := live.CreateFile(t.Context(), live.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
				require.NoError(t, err)
				asset, err := live.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				checkout, operation, err := live.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
				require.NoError(t, err)
				require.NoError(t, live.FinishPhotoCheckout(t.Context(), checkout.ID, operation.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
				checkoutID = checkout.ID
			}
			resp, body := do(t, ts, http.MethodPost, endpoint, nil, input)
			require.Equal(t, http.StatusConflict, resp.StatusCode, body)
			if checkoutID != "" {
				require.Contains(t, body, checkoutID)
			}
			require.Contains(t, body, `"code":"storage_namespace_overlap"`)
		})
	}
}

// TestStorageRegistrationAdmitsReleasedCheckoutDestinationAndSurvivesRestart
// covers a retired checkout whose destination folder has been confirmed
// empty: registering a filesystem store at that same path, or at a path
// nested beneath it, must succeed, and the admission check daemon startup
// runs against every configured path (ingest.CheckoutPathOverlap, the same
// function this registration path uses) must keep admitting it on a later
// restart, after the registered store has written its own ownership marker
// into the folder.
func TestStorageRegistrationAdmitsReleasedCheckoutDestinationAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		nested bool
	}{
		{name: "exact destination reused"},
		{name: "store nested beneath destination", nested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := t.TempDir()
			bindingPath := destination
			if tc.nested {
				bindingPath = filepath.Join(destination, "nested")
			}
			ts, live := newTestServer(t, func(d *api.Deps) {
				d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: bindingPath}}
				d.BlobRegistry = blob.NewRegistry(t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil)
				t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
			})
			node, err := live.CreateFile(t.Context(), live.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
			require.NoError(t, err)
			asset, err := live.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			checkout, operation, err := live.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
			require.NoError(t, err)
			require.NoError(t, live.FinishPhotoCheckout(t.Context(), checkout.ID, operation.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
			_, err = live.RetirePhotoCheckout(t.Context(), checkout.ID)
			require.NoError(t, err)

			resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
				map[string]any{"name": "archive", "binding": "archive"})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			previewed, err := live.PhotoCheckout(t.Context(), checkout.ID)
			require.NoError(t, err)
			require.Equal(t, destination, previewed.Destination)
			var preview api.BlobStorePreview
			require.NoError(t, json.Unmarshal([]byte(body), &preview))
			resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil,
				map[string]any{"preview_token": preview.PreviewToken})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)

			released, err := live.PhotoCheckout(t.Context(), checkout.ID)
			require.NoError(t, err)
			require.Empty(t, released.Destination)

			entries, err := os.ReadDir(bindingPath)
			require.NoError(t, err)
			require.NotEmpty(t, entries, "registration should have written its ownership marker")

			checkouts, err := live.ListPhotoCheckouts(t.Context())
			require.NoError(t, err)
			id, overlap, _, err := ingest.CheckoutPathOverlap(t.Context(), checkouts, bindingPath)
			require.NoError(t, err)
			require.False(t, overlap, "restart admission must not mistake the store's own marker for leftover checkout content: %s", id)
		})
	}
}

func TestStorageRegistrationRejectsVaultOverlap(t *testing.T) {
	t.Parallel()
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

func TestStorageRegistrationRejectsWatchOverlap(t *testing.T) {
	t.Parallel()
	watchRoot := t.TempDir()
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Watches = []config.WatchConfig{{
			Name: "inbox", Source: watchRoot, Destination: "/inbox",
		}}
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"watched": {Kind: "filesystem", Path: watchRoot},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "unsafe", "binding": "watched"})

	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"storage_namespace_overlap"`)
}

func TestRemovedCheckoutBeneathFileAdmitsUnrelatedWatchAndStore(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "parent")
	destination := filepath.Join(parent, "checkout")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	watchSource := t.TempDir()
	var ing *ingest.Ingester
	var vaultRoot string
	ts, live := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: t.TempDir()}}
		d.BlobRegistry = blob.NewRegistry(t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
		ing = &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
		vaultRoot = d.VaultRoot
	})
	node, err := live.CreateFile(t.Context(), live.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := live.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	checkout, op, err := live.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
	require.NoError(t, err)
	require.NoError(t, live.FinishPhotoCheckout(t.Context(), checkout.ID, op.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	_, err = live.RetirePhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(destination))
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.WriteFile(parent, []byte("replacement"), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(watchSource, "arrived.txt"), []byte("watched"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	watcher, err := ingest.NewWatcher(ing, vaultRoot, config.WatchConfig{Name: "unrelated", Source: watchSource, Destination: "/inbox", ScanInterval: config.Duration(time.Second)}, func(fn func() error) error {
		err := fn()
		if err == nil {
			cancel()
		}
		return err
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	err = watcher.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = live.NodeByPath(t.Context(), "/inbox/arrived.txt")
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil, map[string]any{"name": "archive", "binding": "archive"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview api.BlobStorePreview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil, map[string]any{"preview_token": preview.PreviewToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}
