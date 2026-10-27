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

func TestWatchSourcePresenceSymlinkEvidence(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		ing := newTestIngester(t)
		source := writeTree(t, map[string]string{"sub/draft.md": "first"})
		cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true, ScanInterval: config.Duration(time.Second)}
		w, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		require.NoError(t, w.scan(t.Context(), openWatcherRoot(t, w)))
		ref := "sub/draft.md"
		if ancestor {
			ref = "sub"
		}
		require.NoError(t, os.RemoveAll(filepath.Join(source, filepath.FromSlash(ref))))
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(source, filepath.FromSlash(ref))))
		w, err = NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		require.NoError(t, w.scan(t.Context(), openWatcherRoot(t, w)))
		fence, err := ing.Store.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{SourcePresent: true}})
		require.NoError(t, err)
		want := 0
		if ancestor {
			want = 1
		}
		require.Len(t, fence.ContentVersionIDs, want, "a symlinked parent is unlisted; a symlink leaf is absent")
	}
}

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

func TestWatcherAliasCheckPrunesUnreadableExcludedDirectory(t *testing.T) {
	source := t.TempDir()
	excluded := filepath.Join(source, "private")
	require.NoError(t, os.Mkdir(excluded, 0o700))
	require.NoError(t, os.Chmod(excluded, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(excluded, 0o700)) })

	f, err := os.Open(excluded)
	if err == nil {
		_, err = f.ReadDir(-1)
		require.NoError(t, f.Close())
	}
	if err == nil {
		t.Skip("test account can read mode-000 directories")
	}

	watcher, err := NewWatcher(newTestIngester(t), t.TempDir(), config.WatchConfig{
		Name: "excluded-private", Source: source, Destination: "/inbox",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		Exclude: []string{"private"},
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root, err := watcher.openRoot(t.Context())
	require.NoError(t, err)
	require.NoError(t, root.Close())
}
