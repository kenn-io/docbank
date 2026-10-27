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
)

func TestWatcherCaseOnlyRenameKeepsHistory(t *testing.T) {
	for _, tc := range []struct {
		name, oldRef, newRef, renameOld, renameNew string
	}{
		{"leaf", "a.md", "A.md", "a.md", "A.md"},
		{"parent_directory", "notes/a.md", "Notes/a.md", "notes", "Notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ing := newTestIngester(t)
			source := writeTree(t, map[string]string{tc.oldRef: "first"})
			watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
				Name: "notes", Source: source, Destination: "/archive",
				SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
			}, runTestMutation, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			root := openWatcherRoot(t, watcher)
			at := time.Unix(1700000000, 0)
			scan := func(seconds int) {
				t.Helper()
				watcher.now = func() time.Time { return at.Add(time.Duration(seconds) * time.Second) }
				complete, scanErr := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
				require.NoError(t, scanErr)
				require.True(t, complete, "scan must be complete")
			}
			scan(0)
			scan(10)
			original, err := ing.Store.NodeByPath(t.Context(), "/archive/"+tc.oldRef)
			require.NoError(t, err)
			before, beforeInfo := nativeWatchIdentity(t, filepath.Join(source, filepath.FromSlash(tc.oldRef)))
			if !matchableWatchIdentity(before) {
				t.Skip("filesystem did not provide usable native birth evidence")
			}
			require.NoError(t, os.Rename(filepath.Join(source, tc.renameOld), filepath.Join(source, tc.renameNew)))
			oldInfo, err := os.Stat(filepath.Join(source, filepath.FromSlash(tc.oldRef)))
			if os.IsNotExist(err) {
				t.Skip("case-sensitive filesystem: old spelling no longer resolves")
			}
			require.NoError(t, err)
			after, afterInfo := nativeWatchIdentity(t, filepath.Join(source, filepath.FromSlash(tc.newRef)))
			require.Equal(t, before, after)
			require.True(t, os.SameFile(beforeInfo, afterInfo))
			require.True(t, os.SameFile(oldInfo, afterInfo))
			require.True(t, watchOldRefAbsent(root, tc.oldRef))
			for _, seconds := range []int{11, 21, 31} {
				scan(seconds)
			}
			cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			require.Len(t, cursors, 1)
			assert.Equal(t, tc.newRef, cursors[0].Ref, "case-only rename must rebind the source cursor")
			assert.Equal(t, original.ID, cursors[0].NodeID, "rename must retain node identity")
			require.NoError(t, os.WriteFile(filepath.Join(source, filepath.FromSlash(tc.newRef)), []byte("second longer"), 0o644))
			for _, seconds := range []int{32, 42, 52} {
				scan(seconds)
			}
			_, total, err := ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
			require.NoError(t, err)
			cursors, err = ing.Store.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			require.Len(t, cursors, 1)
			assert.Equal(t, tc.newRef, cursors[0].Ref)
			assert.Equal(t, original.ID, cursors[0].NodeID)
			assert.Equal(t, 2, total, "edit after case-only rename must advance content history")
		})
	}
}
