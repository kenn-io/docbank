package api

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestStorageProtectionPathsSkipS3AndRejectUncertainFilesystemCheckout(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	_, err = catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	checkout, operation, err := catalog.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{All: true}, "unavailable\x00checkout", 100)
	require.NoError(t, err)
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkout.ID, operation.ID, store.StorageOperationFailed, "{}", "partial", time.Now().Add(time.Hour)))
	d := Deps{Store: catalog, VaultRoot: t.TempDir(), Cfg: config.Default()}
	d.BlobRegistry = blob.NewRegistry(t.Context(), catalog.VaultID(), nil, nil)
	t.Cleanup(func() { require.NoError(t, d.BlobRegistry.Close()) })
	d.Cfg.Watches = []config.WatchConfig{{Source: "unavailable\x00watch"}}
	paths, err := storageProtectionPaths(t.Context(), d, config.StoreBindingConfig{Kind: "s3", Bucket: "archive", Region: "us-east-1"})
	require.NoError(t, err)
	require.Empty(t, paths)
	d.Cfg.Watches = nil
	_, err = storageProtectionPaths(t.Context(), d, config.StoreBindingConfig{Kind: "filesystem", Path: t.TempDir()})
	require.ErrorContains(t, err, checkout.Destination, "filesystem registration must keep uncertain checkout paths closed")
}
