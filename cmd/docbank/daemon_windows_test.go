//go:build windows

package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
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
	conflicting := cfg
	conflicting.Watches = append(append([]config.WatchConfig{}, cfg.Watches...), config.WatchConfig{Name: "overlap", Source: destination})
	pending, err = validateConfiguredCheckoutStartup(t.Context(), conflicting, stores, catalog)
	require.ErrorContains(t, err, checkout.ID, "a later confirmed overlap must override an earlier access error")
	require.False(t, pending)
	waiting := make(chan error, 1)
	supervisor := jobs.New(t.Context(), nil)
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	require.NoError(t, supervisor.Start("checkout:configured-paths", func(ctx context.Context) error {
		return waitForCheckoutValidation(ctx, func(ctx context.Context) error {
			err := validateConfiguredCheckouts(ctx, cfg, stores, catalog)
			select {
			case waiting <- err:
			default:
			}
			return err
		}, slog.Default())
	}))
	select {
	case err := <-waiting:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("checkout validation did not retry in the supervisor")
	}
	jobs := supervisor.Snapshot()
	require.Len(t, jobs, 1)
	require.Nil(t, jobs[0].FinishedAt)
	require.NoError(t, windows.CloseHandle(handle))
	handle = windows.InvalidHandle
	require.Eventually(t, func() bool { return supervisor.Snapshot()[0].FinishedAt != nil }, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, supervisor.Snapshot()[0].Error)
	pending, err = validateConfiguredCheckoutStartup(t.Context(), cfg, stores, catalog)
	require.NoError(t, err)
	require.False(t, pending)
}
