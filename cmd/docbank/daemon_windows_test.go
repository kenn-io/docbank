//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/sys/windows"
)

func TestConfiguredWatchStoresAllowUnavailableSecondaryDrive(t *testing.T) {
	var unavailableRoot string
	for drive := 'Z'; drive >= 'A'; drive-- {
		candidate := string(drive) + `:\`
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			unavailableRoot = candidate
			break
		}
	}
	if unavailableRoot == "" {
		t.Skip("all Windows drive letters are available")
	}

	cfg := config.Default()
	cfg.Watches = []config.WatchConfig{{
		Name: "inbox", Source: t.TempDir(), Destination: "/inbox",
	}}
	cfg.StoreBindings = map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: filepath.Join(unavailableRoot, "archive")},
	}
	stores := []store.BlobStore{{
		ID:   "10000000-0000-4000-8000-000000000001",
		Name: "archive", Kind: "filesystem", Role: "secondary",
		Lifecycle: "active", Binding: "archive",
	}}

	require.NoError(t, validateConfiguredWatchStores(cfg, stores))
}

func TestConfiguredCheckoutStartupDefersInaccessibleDestination(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	destination := t.TempDir()
	checkout, operation, err := catalog.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
	require.NoError(t, err)
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkout.ID, operation.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	name, err := windows.UTF16PtrFromString(destination)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		if handle != windows.InvalidHandle {
			require.NoError(t, windows.CloseHandle(handle))
		}
	})
	cfg := config.Default()
	cfg.Watches = []config.WatchConfig{{Name: "inbox", Source: t.TempDir()}}
	cfg.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: t.TempDir()}}
	stores := []store.BlobStore{{Name: "archive", Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "archive"}}
	pending, err := validateConfiguredCheckoutStartup(t.Context(), cfg, stores, catalog)
	require.NoError(t, err, "uncertain paths must not prevent daemon startup")
	require.True(t, pending)
	require.NoError(t, windows.CloseHandle(handle))
	handle = windows.InvalidHandle
	pending, err = validateConfiguredCheckoutStartup(t.Context(), cfg, stores, catalog)
	require.NoError(t, err)
	require.False(t, pending)
}
