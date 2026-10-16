package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestStorageProtectionPathsAdmitRemovedCheckoutBeneathFile(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	_, err = catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	parent := filepath.Join(t.TempDir(), "parent")
	destination := filepath.Join(parent, "checkout")
	require.NoError(t, os.MkdirAll(destination, 0o700))
	checkout, operation, _, err := catalog.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{All: true}, destination, 100)
	require.NoError(t, err)
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkout.ID, operation.ID, store.StorageOperationFailed, "{}", "partial", time.Now().Add(time.Hour)))
	_, err = catalog.RetirePhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(destination))
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.WriteFile(parent, nil, 0o600))
	d := Deps{Store: catalog, VaultRoot: t.TempDir(), Cfg: config.Default()}
	d.BlobRegistry = blob.NewRegistry(t.Context(), catalog.VaultID(), nil, nil)
	t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	d.Cfg.Watches = []config.WatchConfig{{Source: filepath.Join(parent, "watch")}}
	paths, err := storageProtectionPaths(t.Context(), d, config.StoreBindingConfig{Kind: "s3", Bucket: "archive", Region: "us-east-1"})
	require.NoError(t, err)
	require.Empty(t, paths)
	d.Cfg.Watches = nil
	bindingPath := t.TempDir()
	paths, err = storageProtectionPaths(t.Context(), d, config.StoreBindingConfig{Kind: "filesystem", Path: bindingPath})
	require.NoError(t, err)
	require.Equal(t, []string{d.VaultRoot, bindingPath}, paths)
}
