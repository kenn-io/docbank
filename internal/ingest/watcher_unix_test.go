//go:build unix

package ingest

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestWatcherDoesNotFollowSwappedDirectoryAncestor(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"sub/inside.txt": "inside"})
	outside := writeTree(t, map[string]string{"outside.txt": "outside"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "confined", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	t0 := time.Now()
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0))
	require.Len(t, watcher.observations, 1)

	swapped := false
	watcher.beforeDescend = func(rel string) {
		if swapped || rel != "sub" {
			return
		}
		swapped = true
		require.NoError(t, os.Rename(
			filepath.Join(source, "sub"), filepath.Join(source, "detached"),
		))
		require.NoError(t, os.Symlink(outside, filepath.Join(source, "sub")))
	}
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(time.Second)))
	assert.True(t, swapped)
	assert.Empty(t, watcher.observations)
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/outside.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
}
