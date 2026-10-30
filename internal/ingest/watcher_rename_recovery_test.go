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

func promptRenameFixture(t *testing.T, mutate func(func() error) error) (*Watcher, *watchRoot, *Ingester, string, int64) {
	t.Helper()
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"draft.md": "first", "unstable/other.md": "other"})
	requireRenameEvidence(t, filepath.Join(source, "draft.md"))
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		ScanInterval: config.Duration(time.Second),
	}, mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, watcher.scan(t.Context(), root))
	node, err := ing.Store.NodeByPath(t.Context(), "/archive/draft.md")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(source, "notes"), 0o755))
	require.NoError(t, os.Rename(filepath.Join(source, "draft.md"), filepath.Join(source, "notes", "final.md")))
	return watcher, root, ing, source, node.ID
}

func TestWatchRenameIncompleteInventoryKeepsAcceptedEvidence(t *testing.T) {
	watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
	outside := t.TempDir()
	watcher.beforeDescend = func(rel string) {
		if rel != "unstable" {
			return
		}
		watcher.beforeDescend = nil
		require.NoError(t, os.Rename(filepath.Join(source, "unstable"), filepath.Join(source, "detached")))
		require.NoError(t, os.Symlink(outside, filepath.Join(source, "unstable")))
	}
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
	require.NoError(t, err)
	assert.False(t, complete)
	assert.NotNil(t, watcher.observations["draft.md"].accepted)
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	assert.Equal(t, "draft.md", cursors[0].Ref)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/notes/final.md")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, os.Remove(filepath.Join(source, "unstable")))
	require.NoError(t, os.Rename(filepath.Join(source, "detached"), filepath.Join(source, "unstable")))
	require.NoError(t, watcher.scan(t.Context(), root))
	cursors, err = ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	found := false
	for _, cursor := range cursors {
		if cursor.Ref == "notes/final.md" {
			found = cursor.NodeID == id
		}
	}
	assert.True(t, found)
}

func TestWatchRenameRecheckFailureDefersDestination(t *testing.T) {
	watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
	moved := filepath.Join(source, "notes", "preserved.md")
	watcher.beforeRebind = func(ref string) {
		watcher.beforeRebind = nil
		require.Equal(t, "notes/final.md", ref)
		require.NoError(t, os.Rename(filepath.Join(source, "notes", "final.md"), moved))
		require.NoError(t, os.WriteFile(filepath.Join(source, "notes", "final.md"), []byte("replacement"), 0o644))
	}
	require.NoError(t, watcher.scan(t.Context(), root))
	assert.NotNil(t, watcher.observations["draft.md"].accepted)
	_, err := ing.Store.NodeByPath(t.Context(), "/archive/notes/final.md")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, os.Remove(filepath.Join(source, "notes", "final.md")))
	require.NoError(t, os.Rename(moved, filepath.Join(source, "notes", "final.md")))
	require.NoError(t, watcher.scan(t.Context(), root))
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	for _, cursor := range cursors {
		if cursor.NodeID == id {
			assert.Equal(t, "notes/final.md", cursor.Ref)
		}
	}
}

func TestWatchRenameAdmissionRechecksIdentityAndOldPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		admit func(*testing.T, string, string)
	}{
		{
			name: "destination replaced during admission",
			admit: func(t *testing.T, source, candidate string) {
				t.Helper()
				preserved := filepath.Join(source, "notes", "preserved.md")
				require.NoError(t, os.Rename(candidate, preserved))
				require.NoError(t, os.WriteFile(candidate, []byte("replacement"), 0o600))
			},
		},
		{
			name: "old path reused during admission",
			admit: func(t *testing.T, source, _ string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(source, "draft.md"), []byte("new source"), 0o600))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
			candidate := filepath.Join(source, "notes", "final.md")
			admitted := false
			watcher.mutate = func(fn func() error) error {
				if !admitted {
					admitted = true
					tc.admit(t, source, candidate)
				}
				return fn()
			}
			require.NoError(t, watcher.scan(t.Context(), root))
			require.True(t, admitted, "rename must pass through the mutation gate")
			accepted, ok := watcher.observations["draft.md"]
			require.True(t, ok, "previous source observation must be retained")
			require.NotNil(t, accepted.accepted)
			require.Equal(t, id, accepted.accepted.nodeID)
			cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			found := false
			for _, cursor := range cursors {
				if cursor.NodeID == id {
					found = true
					assert.Equal(t, "draft.md", cursor.Ref)
				}
			}
			assert.True(t, found, "accepted document must keep its original source reference")
			_, err = ing.Store.NodeByPath(t.Context(), "/archive/notes/final.md")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestWatchRenameStoreFailureDoesNotCreateNewNode(t *testing.T) {
	watcher, root, ing, _, id := promptRenameFixture(t, runTestMutation)
	original, err := ing.Store.NodeByPath(t.Context(), "/archive/draft.md")
	require.NoError(t, err)
	require.Equal(t, id, original.ID)
	_, _, err = ing.Store.Trash(t.Context(), original.ID, original.Revision)
	require.NoError(t, err)
	err = watcher.scan(t.Context(), root)
	require.ErrorContains(t, err, "trashed")
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/notes/final.md")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestWatchDirectoryCollectionFailureRestartsDescendantSettling(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"nested/a.md": "first", "sibling.md": "sibling"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Hour),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, watcher.scan(t.Context(), root))
	path := filepath.Join(source, "nested", "a.md")
	original, err := ing.Store.NodeByPath(t.Context(), "/archive/nested/a.md")
	require.NoError(t, err)
	accepted := *watcher.observations["nested/a.md"].accepted

	watcher.config.CaptureOnChange = false
	require.NoError(t, os.WriteFile(path, []byte("second"), 0o600))
	start := time.Unix(1_700_000_000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, start))
	require.Equal(t, start, watcher.observations["nested/a.md"].stableSince)

	moved := filepath.Join(source, "nested-moved")
	watcher.beforeDescend = func(rel string) {
		if rel == "nested" {
			watcher.beforeDescend = nil
			require.NoError(t, os.Rename(filepath.Join(source, "nested"), moved))
		}
	}
	watcher.now = func() time.Time { return start.Add(9 * time.Second) }
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
	require.NoError(t, err)
	require.False(t, complete)
	require.True(t, watcher.observations["nested/a.md"].settlingInterrupted)
	require.Equal(t, accepted, *watcher.observations["nested/a.md"].accepted)

	require.NoError(t, os.Rename(moved, filepath.Join(source, "nested")))
	recoveredAt := start.Add(20 * time.Second)
	watcher.now = func() time.Time { return recoveredAt }
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	require.Equal(t, recoveredAt, watcher.observations["nested/a.md"].stableSince)
	require.False(t, watcher.observations["nested/a.md"].processed)
	_, count, err := ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, count, "time while the directory could not be collected must not satisfy settling")

	watcher.now = func() time.Time { return start.Add(29 * time.Second) }
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	_, count, err = ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	watcher.now = func() time.Time { return start.Add(30 * time.Second) }
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	_, count, err = ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestWatchOldPathMissingRequiresPinnedRoot(t *testing.T) {
	watcher, root, _, source, _ := promptRenameFixture(t, runTestMutation)
	assert.True(t, watchOldRefAbsent(root, "draft.md"))
	moved := source + "-moved"
	renameWatcherRoot(t, root, moved)
	assert.False(t, watchOldRefAbsent(root, "draft.md"))
	_ = watcher
}

func TestWatchIncompletePassHonorsDirtyHintAndKeepsSubscription(t *testing.T) {
	calls := 0
	mutate := func(fn func() error) error { calls++; return fn() }
	watcher, root, ing, source, _ := promptRenameFixture(t, mutate)
	// Keep the hinted file outside the subtree made unreadable below, whose
	// interruption would otherwise independently reset its processed state.
	require.NoError(t, os.WriteFile(filepath.Join(source, "retained.md"), []byte("retained"), 0o644))
	require.NoError(t, watcher.scan(t.Context(), root))
	retained, err := ing.Store.NodeByPath(t.Context(), "/archive/retained.md")
	require.NoError(t, err)
	require.True(t, watcher.observations["retained.md"].processed)
	notifier := newFakeWatchNotificationSource(source)
	watcher.notifications = notifier
	initial := calls
	outside := t.TempDir()
	watcher.beforeDescend = func(rel string) {
		if rel != "unstable" {
			return
		}
		watcher.beforeDescend = nil
		require.NoError(t, os.Rename(filepath.Join(source, "unstable"), filepath.Join(source, "detached")))
		require.NoError(t, os.Symlink(outside, filepath.Join(source, "unstable")))
	}
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Dirty: map[string]struct{}{"retained.md": {}}})
	require.NoError(t, err)
	require.False(t, complete)
	assert.Equal(t, initial+1, calls, "the hinted file outside the unreadable subtree is processed")
	assert.Equal(t, 0, notifier.endScans, "an incomplete pass keeps subscriptions it did not see")
	require.NotNil(t, watcher.observations["retained.md"].accepted)
	require.Equal(t, retained.ID, watcher.observations["retained.md"].accepted.nodeID)
	require.True(t, watcher.observations["retained.md"].processed)
	require.NoError(t, os.Remove(filepath.Join(source, "unstable")))
	require.NoError(t, os.Rename(filepath.Join(source, "detached"), filepath.Join(source, "unstable")))
	complete, err = watcher.scanWithHints(t.Context(), root, watchScanHints{})
	require.NoError(t, err)
	require.True(t, complete)
	assert.Equal(t, 1, notifier.endScans)
	assert.True(t, watcher.observations["retained.md"].processed)
}

func TestWatchRenameOldPathReuseKeepsDistinctIdentity(t *testing.T) {
	watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
	require.NoError(t, watcher.scan(t.Context(), root))
	require.NoError(t, os.WriteFile(filepath.Join(source, "draft.md"), []byte("new file"), 0o644))
	err := watcher.scan(t.Context(), root)
	require.Error(t, err)
	cursors, cursorErr := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, cursorErr)
	require.Len(t, cursors, 2)
	assert.Equal(t, "notes/final.md", cursors[0].Ref)
	parent, err := ing.Store.MkdirAll(t.Context(), "/organized")
	require.NoError(t, err)
	_, _, err = ing.Store.Move(t.Context(), id, parent.ID, "old.md", store.UnconditionalRev)
	require.NoError(t, err)
	require.NoError(t, watcher.scan(t.Context(), root))
	cursors, err = ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 3)
	assert.Equal(t, "draft.md", cursors[0].Ref)
	assert.NotEqual(t, id, cursors[0].NodeID)
}

func TestWatchKnownSubtreeReplacedBySymlinkReportsBlock(t *testing.T) {
	watcher, root, _, source, _ := promptRenameFixture(t, runTestMutation)
	outside := t.TempDir()
	require.NoError(t, os.Rename(filepath.Join(source, "unstable"), filepath.Join(source, "detached")))
	require.NoError(t, os.Symlink(outside, filepath.Join(source, "unstable")))
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
	require.ErrorIs(t, err, errWatchBlocked)
	assert.False(t, complete)
	assert.NotNil(t, watcher.observations["draft.md"].accepted)
	assert.NotNil(t, watcher.observations["unstable/other.md"].accepted)
}

func TestWatchRenameRecreatedExactOldNameBlocksRebind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create func(source string) error
	}{
		{"hard_link", func(source string) error {
			return os.Link(filepath.Join(source, "notes", "final.md"), filepath.Join(source, "draft.md"))
		}},
		{"symlink", func(source string) error {
			return os.Symlink(filepath.Join(source, "notes", "final.md"), filepath.Join(source, "draft.md"))
		}},
		{"directory", func(source string) error {
			return os.Mkdir(filepath.Join(source, "draft.md"), 0o700)
		}},
		{"distinct_file", func(source string) error {
			return os.WriteFile(filepath.Join(source, "draft.md"), []byte("new source"), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
			watcher.beforeRebind = func(ref string) {
				watcher.beforeRebind = nil
				require.Equal(t, "notes/final.md", ref)
				require.NoError(t, tc.create(source))
			}
			require.NoError(t, watcher.scan(t.Context(), root))
			cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			require.Len(t, cursors, 2)
			refs := make(map[int64]string, len(cursors))
			for _, cursor := range cursors {
				refs[cursor.NodeID] = cursor.Ref
			}
			require.Equal(t, "draft.md", refs[id])
			require.NotNil(t, watcher.observations["draft.md"].accepted)
			require.NoError(t, os.Remove(filepath.Join(source, "draft.md")))
			require.NoError(t, watcher.scan(t.Context(), root))
			cursors, err = ing.Store.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			require.Len(t, cursors, 2)
			for _, cursor := range cursors {
				refs[cursor.NodeID] = cursor.Ref
			}
			require.Equal(t, "notes/final.md", refs[id])
		})
	}
}

func TestWatchRenameOldParentReplacedBeforeRebindDefers(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"notes/a.md": "first"})
	requireRenameEvidence(t, filepath.Join(source, "notes", "a.md"))
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, watcher.scan(t.Context(), root))
	original, err := ing.Store.NodeByPath(t.Context(), "/archive/notes/a.md")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(source, "notes", "a.md"), filepath.Join(source, "final.md")))
	watcher.beforeRebind = func(ref string) {
		watcher.beforeRebind = nil
		require.Equal(t, "final.md", ref)
		require.NoError(t, os.Remove(filepath.Join(source, "notes")))
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(source, "notes")))
	}
	require.NoError(t, watcher.scan(t.Context(), root))
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	require.Equal(t, "notes/a.md", cursors[0].Ref)
	require.Equal(t, original.ID, cursors[0].NodeID)
	require.NoError(t, os.Remove(filepath.Join(source, "notes")))
	require.NoError(t, watcher.scan(t.Context(), root))
	cursors, err = ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	require.Equal(t, "final.md", cursors[0].Ref)
	require.Equal(t, original.ID, cursors[0].NodeID)
}

func TestWatchLeafDirectoryChangesDuringDescentKeepsPassIncomplete(t *testing.T) {
	watcher, root, ing, source, siblingID := watcherFinalPair(t)
	require.NoError(t, os.Remove(filepath.Join(source, "a.md")))
	require.NoError(t, os.Mkdir(filepath.Join(source, "a.md"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	notifier := newFakeWatchNotificationSource(source)
	watcher.notifications = notifier
	watcher.beforeDescend = func(rel string) {
		if rel == "a.md" {
			watcher.beforeDescend = nil
			require.NoError(t, os.Remove(filepath.Join(source, "a.md")))
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(source, "a.md")))
		}
	}
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Dirty: map[string]struct{}{"b.md": {}}})
	require.NoError(t, err)
	require.False(t, complete)
	requireSiblingVersions(t, ing, siblingID, 2)
	require.NotNil(t, watcher.observations["a.md"].accepted)
	require.True(t, watcher.observations["b.md"].processed)
	require.Equal(t, 0, notifier.endScans)
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	requireSiblingVersions(t, ing, siblingID, 2)
	require.Equal(t, 1, notifier.endScans)
}

func TestWatchReplacementOwnershipRequiresSuccessfulIngestion(t *testing.T) {
	for _, acceptBeforeMove := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed only", true: "accepted unchanged bytes"}[acceptBeforeMove], func(t *testing.T) {
			watcher, root, ing, source, _ := watcherFinalPair(t)
			original := *watcher.observations["a.md"].accepted
			replacement := filepath.Join(t.TempDir(), "replacement.md")
			require.NoError(t, os.WriteFile(replacement, []byte("first"), 0o600))
			// Install a different object between scans, retaining the old object
			// outside the source to rule out native object-number reuse.
			require.NoError(t, os.Rename(filepath.Join(source, "a.md"), filepath.Join(t.TempDir(), "original.md")))
			require.NoError(t, os.Rename(replacement, filepath.Join(source, "a.md")))
			identity, _ := nativeWatchIdentity(t, filepath.Join(source, "a.md"))
			require.NotEqual(t, original.identity, identity)
			if acceptBeforeMove && !matchableWatchIdentity(identity) {
				t.Skip("filesystem did not provide usable native birth evidence")
			}
			watcher.config.CaptureOnChange = false
			watcher.config.SettleTime = config.Duration(time.Second)
			now := time.Unix(1_700_000_000, 0)
			watcher.now = func() time.Time { return now }
			requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
			require.False(t, watcher.observations["a.md"].processed)
			require.Equal(t, original, *watcher.observations["a.md"].accepted,
				"complete observation alone must not transfer ownership")
			if acceptBeforeMove {
				now = now.Add(time.Second)
				require.NoError(t, watcher.scan(t.Context(), root))
				require.True(t, watcher.observations["a.md"].processed)
				require.Equal(t, identity, watcher.observations["a.md"].accepted.identity)
				require.Equal(t, original.nodeID, watcher.observations["a.md"].accepted.nodeID)
			}
			requireSiblingVersions(t, ing, original.nodeID, 1)
			require.NoError(t, os.Rename(filepath.Join(source, "a.md"), filepath.Join(source, "final.md")))
			now = now.Add(time.Second)
			require.NoError(t, watcher.scan(t.Context(), root))
			now = now.Add(time.Second)
			require.NoError(t, watcher.scan(t.Context(), root))
			cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			var finalID int64
			for _, cursor := range cursors {
				if cursor.Ref == "final.md" {
					finalID = cursor.NodeID
				}
			}
			require.NotZero(t, finalID)
			if acceptBeforeMove {
				require.Equal(t, original.nodeID, finalID)
				requireWatchArchivedSource(t, ing, original.nodeID, "final.md", "first")
			} else {
				require.NotEqual(t, original.nodeID, finalID)
				requireWatchArchivedSource(t, ing, original.nodeID, "a.md", "first")
				requireWatchArchivedSource(t, ing, finalID, "final.md", "first")
			}
			requireSiblingVersions(t, ing, original.nodeID, 1)
		})
	}
}

func TestWatchIncompletePassDefersOnlyPossibleRenames(t *testing.T) {
	watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
	require.NoError(t, os.WriteFile(filepath.Join(source, "fresh.md"), []byte("unrelated"), 0o644))
	held := true
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if held && file.ref == "unstable/other.md" {
			return watchFileState{}, ErrSourceChanged
		}
		return file.observe(withChange)
	}
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
	require.NoError(t, err)
	require.False(t, complete)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/fresh.md")
	require.NoError(t, err, "an unrelated new file imports during an incomplete pass")
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/notes/final.md")
	require.ErrorIs(t, err, store.ErrNotFound, "a possible rename waits for a complete pass")

	held = false
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	refs := make(map[string]int64)
	for _, cursor := range cursors {
		refs[cursor.Ref] = cursor.NodeID
	}
	assert.Equal(t, id, refs["notes/final.md"], "the complete pass rebinds the renamed source")
	assert.NotContains(t, refs, "draft.md")
}

func TestWatchHeldNewFileDoesNotStallSiblings(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"held.pst": "locked", "report.txt": "ready"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive",
		SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	// ErrSourceChanged stands in for a persistent Windows sharing violation.
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "held.pst" {
			return watchFileState{}, ErrSourceChanged
		}
		return file.observe(withChange)
	}
	root := openWatcherRoot(t, watcher)
	t0 := time.Unix(1_700_000_000, 0)
	for i := range 3 {
		watcher.now = func() time.Time { return t0.Add(time.Duration(i) * time.Second) }
		complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
		require.NoError(t, err)
		require.False(t, complete)
	}
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/report.txt")
	require.NoError(t, err)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/held.pst")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestWatchIncompletePassRestartsSettlingForUnseenFiles(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"pending/draft.md": "first", "held.pst": "locked"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	// An unrelated held file keeps every pass incomplete.
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "held.pst" {
			return watchFileState{}, ErrSourceChanged
		}
		return file.observe(withChange)
	}
	root := openWatcherRoot(t, watcher)
	t0 := time.Unix(1_700_000_000, 0)
	scan := func(at time.Duration) {
		t.Helper()
		watcher.now = func() time.Time { return t0.Add(at) }
		complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
		require.NoError(t, err)
		require.False(t, complete)
	}
	scan(0)
	pending, away := filepath.Join(source, "pending"), filepath.Join(t.TempDir(), "pending")
	require.NoError(t, os.Rename(pending, away))
	scan(5 * time.Second)
	require.NoError(t, os.Rename(away, pending))

	// The settle window started before the folder left has expired.
	scan(11 * time.Second)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/pending/draft.md")
	require.ErrorIs(t, err, store.ErrNotFound, "time unseen must not count toward settling")
	scan(21 * time.Second)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/pending/draft.md")
	require.NoError(t, err)
}

func TestWatchIncompletePassImportsRenameWithOccupiedOldPath(t *testing.T) {
	watcher, root, ing, source, id := promptRenameFixture(t, runTestMutation)
	// Rename matching rejects an occupied old path, so waiting cannot help.
	require.NoError(t, os.Symlink(filepath.Join(source, "notes", "final.md"), filepath.Join(source, "draft.md")))
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "unstable/other.md" {
			return watchFileState{}, ErrSourceChanged
		}
		return file.observe(withChange)
	}
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
	require.NoError(t, err)
	require.False(t, complete)
	separate, err := ing.Store.NodeByPath(t.Context(), "/archive/notes/final.md")
	require.NoError(t, err)
	assert.NotEqual(t, id, separate.ID)
}
