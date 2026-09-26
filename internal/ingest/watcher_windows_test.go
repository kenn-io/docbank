//go:build windows

package ingest

import (
	"io"
	"log/slog"
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
