//go:build windows

package ingest

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestOpenWatchLeafRetriesDirectoryReplacement(t *testing.T) {
	source := writeTree(t, map[string]string{"document.txt": "ready"})
	root, err := os.OpenRoot(source)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	// Model the replacement after openWatchFile has checked the regular file.
	require.NoError(t, root.Remove("document.txt"))
	require.NoError(t, root.Mkdir("document.txt", 0o700))
	file, err := openWatchLeaf(root, "document.txt")
	require.Nil(t, file)
	require.ErrorIs(t, err, ErrSourceChanged)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)

	file, err = openWatchLeaf(root, "missing.txt")
	require.Nil(t, file)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NotErrorIs(t, err, ErrSourceChanged)
}

func TestWatcherImportsLongPath(t *testing.T) {
	ing := newTestIngester(t)
	rel := strings.Repeat("nested-directory/", 20) + "document.txt"
	source := writeTree(t, map[string]string{rel: "long path contents"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "long-path", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	now := time.Now()
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, now.Add(time.Second)))
	node, err := ing.Store.NodeByPath(t.Context(), "/inbox/"+rel)
	require.NoError(t, err)
	content, err := ing.Blobs.Open(node.BlobHash)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, content.Close()) })
	data, err := io.ReadAll(content)
	require.NoError(t, err)
	assert.Equal(t, "long path contents", string(data))
}

func TestWatcherRetriesExclusivelyOpenedFile(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"document.txt": "ready"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "exclusive", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)

	path16, err := windows.UTF16PtrFromString(filepath.Join(source, "document.txt"))
	require.NoError(t, err)
	handle, err := windows.CreateFile(path16, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(t, windows.CloseHandle(handle))
		}
	})

	t0 := time.Now()
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0))
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(time.Second)))
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
	require.ErrorIs(t, err, store.ErrNotFound,
		"an exclusively held file must remain unprocessed instead of stopping the watcher")
	assert.Empty(t, watcher.observations,
		"a failed locked-file read must restart the settle window")
	require.NoError(t, windows.CloseHandle(handle))
	closed = true

	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(2*time.Second)))
	assert.Len(t, watcher.observations, 1)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(3*time.Second)))
	node, err := ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
	require.NoError(t, err)
	assert.Equal(t, "document.txt", node.Name)
}

func TestWatcherAliasCheckIgnoresExclusivelyOpenedDirectory(t *testing.T) {
	source := writeTree(t, map[string]string{"private/document.txt": "ready"})
	path16, err := windows.UTF16PtrFromString(filepath.Join(source, "private"))
	require.NoError(t, err)
	handle, err := windows.CreateFile(path16, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, windows.CloseHandle(handle)) })

	watcher, err := NewWatcher(newTestIngester(t), t.TempDir(), config.WatchConfig{
		Name: "exclusive-directory", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root, err := watcher.openRoot(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.Close())
}

func TestWatcherRejectsUnreadableReleasedCheckout(t *testing.T) {
	ing := newTestIngester(t)
	source := t.TempDir()
	destination := filepath.Join(source, "checkout")
	require.NoError(t, os.Mkdir(destination, 0o700))
	node, err := ing.Store.CreateFile(t.Context(), ing.Store.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := ing.Store.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	checkout, op, err := ing.Store.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
	require.NoError(t, err)
	require.NoError(t, ing.Store.FinishPhotoCheckout(t.Context(), checkout.ID, op.ID, store.StorageOperationFailed, "{}", "", time.Now().Add(time.Hour)))
	name, err := windows.UTF16PtrFromString(destination)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		if handle != windows.InvalidHandle {
			require.NoError(t, windows.CloseHandle(handle))
		}
	})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{Name: "checkout-watch", Source: source, Destination: "/inbox"}, runTestMutation, nil)
	require.NoError(t, err)
	_, err = watcher.openRoot(t.Context())
	require.Error(t, err)
	require.NoError(t, windows.CloseHandle(handle))
	handle = windows.InvalidHandle
	root, err := watcher.openRoot(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, os.Remove(destination))
	root, err = watcher.openRoot(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.Close())
}
