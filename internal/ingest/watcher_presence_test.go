package ingest

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchSourcePresenceDeleteRecreateAndRestart(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"draft.md": "first"})
	cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true, ScanInterval: config.Duration(time.Second)}
	watch := func() (*Watcher, *watchRoot) {
		t.Helper()
		w, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		return w, openWatcherRoot(t, w)
	}
	w, root := watch()
	require.NoError(t, w.scan(t.Context(), root))
	node, err := ing.Store.NodeByPath(t.Context(), "/archive/draft.md")
	require.NoError(t, err)
	count := func(want int) {
		t.Helper()
		fence, err := ing.Store.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{SourcePresent: true}})
		require.NoError(t, err)
		require.Len(t, fence.ContentVersionIDs, want)
	}
	count(1)
	require.NoError(t, os.Remove(filepath.Join(source, "draft.md")))
	w, root = watch() // deletion while stopped must still be detected
	require.NoError(t, w.scan(t.Context(), root))
	count(0)
	require.NoError(t, os.Mkdir(filepath.Join(source, "draft.md"), 0o700))
	require.NoError(t, w.scan(t.Context(), root))
	count(0) // a directory never establishes regular-file presence
	require.NoError(t, os.Remove(filepath.Join(source, "draft.md")))
	require.NoError(t, os.WriteFile(filepath.Join(source, "draft.md"), []byte("second"), 0o600))
	require.NoError(t, w.scan(t.Context(), root))
	count(1)
	after, err := ing.Store.NodeByPath(t.Context(), "/archive/draft.md")
	require.NoError(t, err)
	require.Equal(t, node.ID, after.ID)
	_, total, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	// No live identity evidence survives a restart: copy/delete stays separate.
	require.NoError(t, os.WriteFile(filepath.Join(source, "copy.md"), []byte("second"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(source, "draft.md")))
	w, root = watch()
	require.NoError(t, w.scan(t.Context(), root))
	copied, err := ing.Store.NodeByPath(t.Context(), "/archive/copy.md")
	require.NoError(t, err)
	require.NotEqual(t, node.ID, copied.ID)
	count(1)
}

func TestWatchSourcePresenceSkippedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, ref, exclude string
		foreign            bool
	}{
		{"excluded leaf", "draft.md", "draft.md", false},
		{"excluded parent", "sub/draft.md", "sub", false},
		{"foreign leaf", "draft.md", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, marked := range []bool{false, true} {
				ing := newTestIngester(t)
				source := writeTree(t, map[string]string{tc.ref: "first"})
				cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true, ScanInterval: config.Duration(time.Second)}
				w, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
				require.NoError(t, err)
				require.NoError(t, w.scan(t.Context(), openWatcherRoot(t, w)))
				if marked {
					parent := filepath.ToSlash(filepath.Dir(tc.ref))
					if parent == "." {
						parent = ""
					}
					require.NoError(t, ing.Store.ReconcileWatchSourcePresence(t.Context(), "notes", store.WatchPresenceScan{ListedDirs: []string{parent}}, time.Now()))
				}
				if tc.exclude != "" {
					cfg.Exclude = []string{tc.exclude}
				}
				w, err = NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
				require.NoError(t, err)
				root := openWatcherRoot(t, w)
				if tc.foreign {
					w.sourceMount++
				}
				require.NoError(t, w.scan(t.Context(), root))
				fence, err := ing.Store.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{SourcePresent: true}})
				require.NoError(t, err)
				want := 1
				if marked {
					want = 0
				}
				require.Len(t, fence.ContentVersionIDs, want)
			}
		})
	}
}

func TestWatchSourcePresenceDeletedExcludedLeafStaysUnchangedAfterRestart(t *testing.T) {
	for _, initiallyMarked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked", true: "marked"}[initiallyMarked], func(t *testing.T) {
			ing := newTestIngester(t)
			source := writeTree(t, map[string]string{"draft.md": "first"})
			cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true, ScanInterval: config.Duration(time.Second)}
			first, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			require.NoError(t, first.scan(t.Context(), openWatcherRoot(t, first)))
			require.NoError(t, os.Remove(filepath.Join(source, "draft.md")))
			if initiallyMarked {
				require.NoError(t, ing.Store.ReconcileWatchSourcePresence(t.Context(), "notes", store.WatchPresenceScan{ListedDirs: []string{""}}, time.Now()))
			}

			cfg.Exclude = []string{"draft.md"}
			restarted, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			require.NoError(t, restarted.scan(t.Context(), openWatcherRoot(t, restarted)))
			fence, err := ing.Store.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{SourcePresent: true}})
			require.NoError(t, err)
			want := 1
			if initiallyMarked {
				want = 0
			}
			require.Len(t, fence.ContentVersionIDs, want, "an excluded cursor keeps its previous presence mark")
		})
	}
}

func TestWatchSourcePresenceForeignMountDirectoryStaysUnchangedAfterRestart(t *testing.T) {
	for _, initiallyMarked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked", true: "marked"}[initiallyMarked], func(t *testing.T) {
			ing := newTestIngester(t)
			source := writeTree(t, map[string]string{"draft.md": "first"})
			cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true, ScanInterval: config.Duration(time.Second)}
			first, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			require.NoError(t, first.scan(t.Context(), openWatcherRoot(t, first)))
			require.NoError(t, os.Remove(filepath.Join(source, "draft.md")))
			require.NoError(t, os.Mkdir(filepath.Join(source, "draft.md"), 0o700))
			if initiallyMarked {
				require.NoError(t, ing.Store.ReconcileWatchSourcePresence(t.Context(), "notes", store.WatchPresenceScan{ListedDirs: []string{""}}, time.Now()))
			}

			restarted, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			root := openWatcherRoot(t, restarted)
			restarted.sourceMount++ // Treat the replacement directory as a mount boundary.
			require.NoError(t, restarted.scan(t.Context(), root))
			fence, err := ing.Store.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{SourcePresent: true}})
			require.NoError(t, err)
			want := 1
			if initiallyMarked {
				want = 0
			}
			require.Len(t, fence.ContentVersionIDs, want, "a directory on another mount does not establish source absence")
		})
	}
}

func TestWatchSourcePresenceDeletedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name             string
		restart, exclude bool
		want             int
	}{
		{name: "live", want: 1},
		{name: "restart", restart: true, want: 1},
		{name: "excluded", restart: true, exclude: true, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ing := newTestIngester(t)
			source := writeTree(t, map[string]string{"keep.md": "kept", "sub/draft.md": "first", "sub/deep/note.md": "nested"})
			cfg := config.WatchConfig{Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true, ScanInterval: config.Duration(time.Second)}
			w, err := NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			root := openWatcherRoot(t, w)
			require.NoError(t, w.scan(t.Context(), root))
			require.NoError(t, os.RemoveAll(filepath.Join(source, "sub")))
			if tc.exclude {
				cfg.Exclude = []string{"sub"}
			}
			if tc.restart {
				w, err = NewWatcher(ing, t.TempDir(), cfg, runTestMutation, slog.New(slog.DiscardHandler))
				require.NoError(t, err)
				root = openWatcherRoot(t, w)
			}
			require.NoError(t, w.scan(t.Context(), root))
			fence, err := ing.Store.ResolveProcessingSourceFence(t.Context(), store.ProcessingSourceFenceRequest{Filters: &store.SearchOptions{SourcePresent: true}})
			require.NoError(t, err)
			require.Len(t, fence.ContentVersionIDs, tc.want)
		})
	}
}
