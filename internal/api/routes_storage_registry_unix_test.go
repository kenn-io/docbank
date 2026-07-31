//go:build unix

package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
)

func TestStorageRegistrationPreviewDoesNotRepairExistingNamespace(t *testing.T) {
	namespace := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, os.Mkdir(namespace, 0o700))
	require.NoError(t, os.Chmod(namespace, 0o755))
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.StoreBindings = map[string]config.StoreBindingConfig{
			"archive": {Kind: "filesystem", Path: namespace},
		}
		d.BlobRegistry = blob.NewRegistry(
			t.Context(), d.Store.VaultID(), d.Cfg.StoreBindings, nil,
		)
		t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/stores/preview", nil,
		map[string]any{"name": "archive", "binding": "archive"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	info, err := os.Stat(namespace)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	var preview api.BlobStorePreview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/stores", nil,
		map[string]any{"preview_token": preview.PreviewToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	info, err = os.Stat(namespace)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
