package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func watcherFinalPair(t *testing.T) (*Watcher, *watchRoot, *Ingester, string, int64) {
	t.Helper()
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"a.md": "first", "b.md": "sibling"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		ScanInterval: config.Duration(50 * time.Millisecond),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, watcher.scan(t.Context(), root))
	sibling, err := ing.Store.NodeByPath(t.Context(), "/archive/b.md")
	require.NoError(t, err)
	return watcher, root, ing, source, sibling.ID
}

func requireSiblingVersions(t *testing.T, ing *Ingester, id int64, want int) {
	t.Helper()
	_, total, err := ing.Store.ContentVersions(t.Context(), id, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, want, total)
}

func TestWatchRetryableObservationRestartsSettleAndKeepsAcceptedOwnership(t *testing.T) {
	watcher, root, ing, source, _ := watcherFinalPair(t)
	path := filepath.Join(source, "a.md")
	original, err := ing.Store.NodeByPath(t.Context(), "/archive/a.md")
	require.NoError(t, err)
	accepted := *watcher.observations["a.md"].accepted

	watcher.config.CaptureOnChange = false
	watcher.config.SettleTime = config.Duration(10 * time.Second)
	require.NoError(t, os.WriteFile(path, []byte("second"), 0o600))
	start := time.Unix(1_700_000_000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, start))
	require.Equal(t, start, watcher.observations["a.md"].stableSince)
	require.False(t, watcher.observations["a.md"].processed)

	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "a.md" {
			return watchFileState{}, fs.ErrNotExist
		}
		return file.observe(withChange)
	}
	watcher.now = func() time.Time { return start.Add(9 * time.Second) }
	complete, err := watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})
	require.NoError(t, err)
	require.False(t, complete)
	require.Equal(t, accepted, *watcher.observations["a.md"].accepted)

	watcher.observeFile = nil
	recoveredAt := start.Add(20 * time.Second)
	watcher.now = func() time.Time { return recoveredAt }
	requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
	require.Equal(t, recoveredAt, watcher.observations["a.md"].stableSince)
	require.False(t, watcher.observations["a.md"].processed)
	_, count, err := ing.Store.ContentVersions(t.Context(), original.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, count, "time while observation failed must not satisfy the settle window")
	require.Equal(t, accepted, *watcher.observations["a.md"].accepted,
		"retryable observations must preserve accepted rename ownership")

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

func TestWatchPermanentObservationErrorSurfacesAndKeepsPending(t *testing.T) {
	watcher, root, ing, source, siblingID := watcherFinalPair(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	watcher.observeFile = func(source watchSource, withChange bool) (watchFileState, error) {
		if source.ref == "a.md" {
			return watchFileState{}, fs.ErrPermission
		}
		return source.observe(withChange)
	}
	err := watcher.scan(t.Context(), root)
	require.ErrorIs(t, err, fs.ErrPermission)
	assert.NotNil(t, watcher.observations["a.md"].accepted)
	requireSiblingVersions(t, ing, siblingID, 1)
	watcher.observeFile = nil
	require.NoError(t, watcher.scan(t.Context(), root))
	requireSiblingVersions(t, ing, siblingID, 2)
}

func TestWatchKnownLeafSymlinkAllowsSiblingAndKeepsArchive(t *testing.T) {
	watcher, root, ing, source, siblingID := watcherFinalPair(t)
	requireRenameEvidence(t, filepath.Join(source, "a.md"))
	original := filepath.Join(source, "saved.md")
	require.NoError(t, os.Rename(filepath.Join(source, "a.md"), original))
	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(source, "a.md")))
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	originalNode, err := ing.Store.NodeByPath(t.Context(), "/archive/a.md")
	require.NoError(t, err)
	notifier := newFakeWatchNotificationSource(source)
	watcher.notifications = notifier
	for range 3 {
		requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
		requireSiblingVersions(t, ing, siblingID, 2)
		requireWatchArchivedSource(t, ing, originalNode.ID, "a.md", "first")
	}
	require.Equal(t, 3, notifier.endScans)
	savedNode, err := ing.Store.NodeByPath(t.Context(), "/archive/saved.md")
	require.NoError(t, err)
	require.NotEqual(t, originalNode.ID, savedNode.ID, "occupied old path must prevent a rename join")
	requireWatchArchivedSource(t, ing, savedNode.ID, "saved.md", "first")
	_, retained := watcher.observations["a.md"]
	require.False(t, retained, "normal complete-pass cleanup must not retain a delayed alias")
	require.NotNil(t, watcher.observations["saved.md"].accepted)
	require.Equal(t, savedNode.ID, watcher.observations["saved.md"].accepted.nodeID)
	require.NoError(t, os.Remove(filepath.Join(source, "a.md")))
	require.NoError(t, os.Rename(original, filepath.Join(source, "a.md")))
	// The moved object already owns a separate cursor. Ordinary cleanup does
	// not promise a later join back into the occupied original destination.
	require.ErrorContains(t, watcher.scan(t.Context(), root), "occupied")
	requireWatchArchivedSource(t, ing, originalNode.ID, "a.md", "first")
	requireWatchArchivedSource(t, ing, savedNode.ID, "saved.md", "first")
	requireSiblingVersions(t, ing, siblingID, 2)
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/outside.md")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestWatchPermanentNativeObservationErrorSurfaces(t *testing.T) {
	watcher, root, ing, source, siblingID := watcherFinalPair(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	nativeErr := errors.New("native mount query failed")
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "a.md" {
			return watchFileState{}, nativeErr
		}
		return file.observe(withChange)
	}
	err := watcher.scan(t.Context(), root)
	require.ErrorIs(t, err, nativeErr)
	requireSiblingVersions(t, ing, siblingID, 1)
	assert.NotNil(t, watcher.observations["a.md"].accepted)
	watcher.observeFile = nil
	require.NoError(t, watcher.scan(t.Context(), root))
	requireSiblingVersions(t, ing, siblingID, 2)
}

func TestWatchCollectionErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{fs.ErrNotExist, true},
		{ErrSourceChanged, true},
		{errors.Join(ErrSourceChanged, fs.ErrNotExist), true},
		{fs.ErrPermission, false},
		{errors.New("native mount failure"), false},
		{errors.Join(ErrSourceChanged, fs.ErrPermission), true},
	} {
		assert.Equal(t, tc.retry, retryableWatchCollectionError(tc.err), "%v", tc.err)
	}
}

func TestWatchKnownBlockReportsJobAndClearsOnRecovery(t *testing.T) {
	watcher, root, ing, source, siblingID := watcherFinalPair(t)
	watcher.newNotifications = func(root string, _ exclusions) (watchNotificationSource, error) {
		return newFakeWatchNotificationSource(root), nil
	}
	known := filepath.Join(source, "known")
	require.NoError(t, os.Mkdir(known, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(known, "note.md"), []byte("nested source"), 0o600))
	require.NoError(t, watcher.scan(t.Context(), root))
	saved := filepath.Join(t.TempDir(), "saved")
	require.NoError(t, os.Rename(known, saved))
	require.NoError(t, os.Symlink(t.TempDir(), known))
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	require.NoError(t, supervisor.Start("watch:notes", func(ctx context.Context) error {
		return watcher.run(ctx, root, nil)
	}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, supervisor.Shutdown(ctx))
	})
	require.Eventually(t, func() bool {
		snap := supervisor.Snapshot()
		return len(snap) == 1 && snap[0].Status == jobs.StatusRunning &&
			snap[0].Error != "" && strings.Contains(snap[0].Error, "known")
	}, 5*time.Second, 20*time.Millisecond)
	requireSiblingVersions(t, ing, siblingID, 2) // the unrelated sibling still imports
	require.NoError(t, os.Remove(known))
	require.NoError(t, os.Rename(saved, known))
	require.Eventually(t, func() bool {
		snap := supervisor.Snapshot()
		_, total, err := ing.Store.ContentVersions(t.Context(), siblingID, 10, 0)
		return err == nil && total == 2 && len(snap) == 1 &&
			snap[0].Status == jobs.StatusRunning && snap[0].Error == ""
	}, 5*time.Second, 20*time.Millisecond)
}

func TestWatchKnownStructuralVariants(t *testing.T) {
	t.Run("file became directory", func(t *testing.T) {
		watcher, root, ing, source, siblingID := watcherFinalPair(t)
		saved := filepath.Join(t.TempDir(), "saved.md")
		require.NoError(t, os.Rename(filepath.Join(source, "a.md"), saved))
		require.NoError(t, os.Mkdir(filepath.Join(source, "a.md"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
		originalNode, err := ing.Store.NodeByPath(t.Context(), "/archive/a.md")
		require.NoError(t, err)
		requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
		requireSiblingVersions(t, ing, siblingID, 2)
		requireWatchArchivedSource(t, ing, originalNode.ID, "a.md", "first")
		require.NoError(t, os.Remove(filepath.Join(source, "a.md")))
		require.NoError(t, os.Rename(saved, filepath.Join(source, "a.md")))
		require.NoError(t, watcher.scan(t.Context(), root))
		requireSiblingVersions(t, ing, siblingID, 2)
	})
	t.Run("file became foreign directory", func(t *testing.T) {
		watcher, root, ing, source, siblingID := watcherFinalPair(t)
		require.NoError(t, os.Remove(filepath.Join(source, "a.md")))
		require.NoError(t, os.Mkdir(filepath.Join(source, "a.md"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
		// Change the admitted mount at the existing mount-comparison seam.
		// This checks the directory gate without mounting a real filesystem.
		originalMount := watcher.sourceMount
		watcher.sourceMount++
		err := watcher.scan(t.Context(), root)
		require.ErrorIs(t, err, errWatchBlocked)
		require.ErrorContains(t, err, `"a.md" crossed a filesystem boundary`)
		requireSiblingVersions(t, ing, siblingID, 1)
		watcher.sourceMount = originalMount
		require.NoError(t, watcher.scan(t.Context(), root))
		requireSiblingVersions(t, ing, siblingID, 2)
	})

	t.Run("file crossed mount", func(t *testing.T) {
		watcher, root, ing, source, siblingID := watcherFinalPair(t)
		require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
		watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
			state, err := file.observe(withChange)
			if file.ref == "a.md" {
				state.mount++
			}
			return state, err
		}
		err := watcher.scan(t.Context(), root)
		require.ErrorIs(t, err, errWatchBlocked)
		requireSiblingVersions(t, ing, siblingID, 2) // only the blocked file waits
		require.NotNil(t, watcher.observations["a.md"].accepted)
		watcher.observeFile = nil
		require.NoError(t, watcher.scan(t.Context(), root))
		requireSiblingVersions(t, ing, siblingID, 2)
	})
}

func TestWatchNestedPermanentJoinedErrorKeepsEvidenceAndSiblingPending(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"nested/a.md": "first", "b.md": "sibling"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		ScanInterval: config.Duration(50 * time.Millisecond),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	require.NoError(t, watcher.scan(t.Context(), root))
	sibling, err := ing.Store.NodeByPath(t.Context(), "/archive/b.md")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "nested/a.md" {
			return watchFileState{}, errors.Join(errors.New("native mount query failed"), fs.ErrPermission)
		}
		return file.observe(withChange)
	}
	err = watcher.scan(t.Context(), root)
	require.ErrorIs(t, err, fs.ErrPermission)
	assert.NotNil(t, watcher.observations["nested/a.md"].accepted)
	requireSiblingVersions(t, ing, sibling.ID, 1)
	watcher.observeFile = nil
	require.NoError(t, watcher.scan(t.Context(), root))
	requireSiblingVersions(t, ing, sibling.ID, 2)
}

// stepWatcherFinalRunner pauses the real runner before each scan. Advancing to
// the next pause makes job, log and observation assertions race-free, including
// when notification failure leaves only periodic reconciliation available.
func stepWatcherFinalRunner(t *testing.T, watcher *Watcher, root *watchRoot) (func() jobs.Snapshot, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	watcher.logger = slog.New(slog.NewTextHandler(&logs, nil))
	watcher.config.ScanInterval = config.Duration(time.Millisecond)
	watcher.newNotifications = func(root string, _ exclusions) (watchNotificationSource, error) {
		return newFakeWatchNotificationSource(root), nil
	}
	ready, resume := make(chan struct{}), make(chan struct{})
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	require.NoError(t, supervisor.Start("watch:notes", func(ctx context.Context) error {
		return watcher.run(ctx, root, func() {
			select {
			case ready <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case <-resume:
			case <-ctx.Done():
			}
		})
	}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, supervisor.Shutdown(ctx))
	})
	awaitPause := func() {
		t.Helper()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("watch runner did not reach the next scan")
		}
	}
	awaitPause()
	return func() jobs.Snapshot {
		t.Helper()
		resume <- struct{}{}
		awaitPause()
		snap := supervisor.Snapshot()
		require.Len(t, snap, 1)
		require.Equal(t, jobs.StatusRunning, snap[0].Status)
		return snap[0]
	}, &logs
}

func TestWatchIncompleteRunnerReportsAndImportsSibling(t *testing.T) {
	watcher, root, ing, source, siblingID := watcherFinalPair(t)
	accepted := *watcher.observations["a.md"].accepted
	require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
	// ErrSourceChanged represents retryable observation failures, including a
	// Windows exclusive hold, without claiming native Windows runtime coverage.
	observationErr := ErrSourceChanged
	watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
		if file.ref == "a.md" && observationErr != nil {
			return watchFileState{}, observationErr
		}
		return file.observe(withChange)
	}
	step, logs := stepWatcherFinalRunner(t, watcher, root)
	notifier, ok := watcher.notifications.(*fakeWatchNotificationSource)
	require.True(t, ok)
	for range 3 {
		snap := step()
		require.Contains(t, snap.Error, "incomplete")
		require.Contains(t, snap.Error, "notes")
		requireSiblingVersions(t, ing, siblingID, 2) // a held file does not stall its siblings
		require.Equal(t, accepted, *watcher.observations["a.md"].accepted)
		require.Equal(t, 0, notifier.endScans)
	}
	require.Equal(t, 1, strings.Count(logs.String(), "watch source blocks reconciliation"))
	observationErr = nil
	require.Empty(t, step().Error)
	requireSiblingVersions(t, ing, siblingID, 2)
	sibling, err := ing.Store.NodeByID(t.Context(), siblingID)
	require.NoError(t, err)
	content, err := ing.Blobs.Open(sibling.BlobHash)
	require.NoError(t, err)
	current, readErr := io.ReadAll(content)
	require.NoError(t, content.Close())
	require.NoError(t, readErr)
	require.Equal(t, "changed sibling", string(current))
	require.True(t, watcher.observations["b.md"].processed)
	require.Equal(t, 1, notifier.endScans)
	require.Equal(t, 1, strings.Count(logs.String(), "watch source reconciliation restored"))
	require.Empty(t, step().Error)
	requireSiblingVersions(t, ing, siblingID, 2)
	require.Equal(t, 1, strings.Count(logs.String(), "watch source reconciliation restored"))
}

func TestWatchIncompleteRunnerRecoveryPreservesNotificationError(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "source first", true: "notifications first"}[failFirst], func(t *testing.T) {
			watcher, root, ing, source, siblingID := watcherFinalPair(t)
			require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte("changed sibling"), 0o600))
			held := !failFirst
			watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
				if file.ref == "a.md" && held {
					return watchFileState{}, ErrSourceChanged
				}
				return file.observe(withChange)
			}
			step, logs := stepWatcherFinalRunner(t, watcher, root)
			if !failFirst {
				require.Contains(t, step().Error, "incomplete")
			}
			watcher.notificationErr = errors.New("synthetic notification backend failure")
			notificationError := step().Error
			require.Contains(t, notificationError, "notifications")
			held = true
			for range 2 {
				require.Equal(t, notificationError, step().Error)
			}
			held = false
			require.Equal(t, notificationError, step().Error)
			requireSiblingVersions(t, ing, siblingID, 2)
			require.Equal(t, 1, strings.Count(logs.String(), "watch source blocks reconciliation"))
			require.Equal(t, 1, strings.Count(logs.String(), "watch source reconciliation restored"))
			require.Equal(t, 1, strings.Count(logs.String(), "watch notifications unavailable"))
			require.NotContains(t, logs.String(), "watch notifications restored")
		})
	}
}

// Consecutive incomplete inventories form one reported incident. A complete
// inventory ends it, so arbitrary alternation must neither hide a new incident
// nor multiply warnings for repetitions within one incident.
func FuzzWatchRunnerIncompleteReportingTransitions(f *testing.F) {
	f.Add([]byte{}, false)
	f.Add([]byte{}, true)
	f.Add([]byte{1, 1, 0, 0, 1, 0}, false)
	f.Add([]byte{1, 1, 0, 0, 1, 0}, true)
	f.Add([]byte{2, 3, 1, 0, 2, 0}, false)
	f.Add([]byte{2, 3, 1, 0, 2, 0}, true)
	f.Fuzz(func(t *testing.T, data []byte, prompt bool) {
		watcher, root, _, _, _ := watcherFinalPair(t)
		watcher.config.CaptureOnChange = prompt
		var observationErr error
		watcher.observeFile = func(file watchSource, withChange bool) (watchFileState, error) {
			if file.ref == "a.md" && observationErr != nil {
				return watchFileState{}, observationErr
			}
			return file.observe(withChange)
		}
		step, logs := stepWatcherFinalRunner(t, watcher, root)
		// Bound filesystem work when materializing the generated sequence.
		if len(data) > 32 {
			data = data[:32]
		}
		incidents, recoveries := 0, 0
		incomplete := make([]bool, 0, len(data)+1)
		for _, value := range data {
			incomplete = append(incomplete, value%4 != 0)
		}
		incomplete = append(incomplete, false) // every generated history recovers
		for i, failed := range incomplete {
			observationErr = nil
			if failed {
				// Exercise both retryable observation classes and joins of them.
				observationErr = []error{ErrSourceChanged, fs.ErrNotExist,
					errors.Join(ErrSourceChanged, fs.ErrNotExist)}[data[i]%3]
				if i == 0 || !incomplete[i-1] {
					incidents++
				}
			} else if i > 0 && incomplete[i-1] {
				recoveries++
			}
			snap := step()
			if failed {
				require.Contains(t, snap.Error, "incomplete", "history %v at %d", data, i)
			} else {
				require.Empty(t, snap.Error, "history %v at %d", data, i)
			}
			require.Equal(t, incidents, strings.Count(logs.String(), "watch source blocks reconciliation"))
			require.Equal(t, recoveries, strings.Count(logs.String(), "watch source reconciliation restored"))
		}
	})
}

func requireWatchArchivedSource(t *testing.T, ing *Ingester, id int64, ref, want string) {
	t.Helper()
	node, err := ing.Store.NodeByID(t.Context(), id)
	require.NoError(t, err)
	content, err := ing.Blobs.Open(node.BlobHash)
	require.NoError(t, err)
	data, readErr := io.ReadAll(content)
	require.NoError(t, content.Close())
	require.NoError(t, readErr)
	require.Equal(t, want, string(data))
	cursors, err := ing.Store.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	for _, cursor := range cursors {
		if cursor.NodeID == id {
			require.Equal(t, ref, cursor.Ref)
			return
		}
	}
	t.Fatal("archived source lost its durable cursor")
}

func FuzzWatchLeafOccupancyPreservesArchiveAndSibling(f *testing.F) {
	f.Add([]byte("changed"), false, byte(0), true)
	f.Add([]byte{}, true, byte(0), false)
	f.Add([]byte("nested"), false, byte(3), false)
	f.Add([]byte("target"), true, byte(255), true)
	f.Fuzz(func(t *testing.T, data []byte, symlink bool, count byte, prompt bool) {
		watcher, root, ing, source, siblingID := watcherFinalPair(t)
		watcher.config.CaptureOnChange = prompt
		original, err := ing.Store.NodeByPath(t.Context(), "/archive/a.md")
		require.NoError(t, err)
		kept, err := ing.Store.MkdirAll(t.Context(), "/kept")
		require.NoError(t, err)
		_, _, err = ing.Store.Move(t.Context(), original.ID, kept.ID, "a.md", store.UnconditionalRev)
		require.NoError(t, err)
		require.NoError(t, os.Rename(filepath.Join(source, "a.md"), filepath.Join(source, "moved.md")))
		// Bound bytes and directory entries only when materializing input.
		if len(data) > 256 {
			data = data[:256]
		}
		changed := "changed:" + string(data)
		require.NoError(t, os.WriteFile(filepath.Join(source, "b.md"), []byte(changed), 0o600))
		if symlink {
			target := filepath.Join(t.TempDir(), "outside.md")
			require.NoError(t, os.WriteFile(target, data, 0o600))
			require.NoError(t, os.Symlink(target, filepath.Join(source, "a.md")))
		} else {
			require.NoError(t, os.Mkdir(filepath.Join(source, "a.md"), 0o700))
			for i := range int(count) % 8 {
				require.NoError(t, os.WriteFile(filepath.Join(source, "a.md", fmt.Sprintf("child-%d.md", i)), data, 0o600))
			}
		}
		for range 2 {
			requireScanComplete(t, checkedScan(watcher.scanWithHints(t.Context(), root, watchScanHints{Reconcile: true})))
			requireSiblingVersions(t, ing, siblingID, 2)
			requireWatchArchivedSource(t, ing, original.ID, "a.md", "first")
			requireWatchArchivedSource(t, ing, siblingID, "b.md", changed)
		}
		moved, err := ing.Store.NodeByPath(t.Context(), "/archive/moved.md")
		require.NoError(t, err)
		require.NotEqual(t, original.ID, moved.ID, "occupied old path must not join the moved object")
		if !symlink {
			for i := range int(count) % 8 {
				child, err := ing.Store.NodeByPath(t.Context(), fmt.Sprintf("/archive/a.md/child-%d.md", i))
				require.NoError(t, err)
				requireWatchArchivedSource(t, ing, child.ID, fmt.Sprintf("a.md/child-%d.md", i), string(data))
			}
		}
	})
}

func TestWatchStructuralBlockRestartsHiddenSettling(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"sub/draft.md": "first"})
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive",
		SettleTime: config.Duration(10 * time.Second), ScanInterval: config.Duration(time.Second),
	}, runTestMutation, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	root := openWatcherRoot(t, watcher)
	t0 := time.Unix(1_700_000_000, 0)
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0))

	sub, hidden := filepath.Join(source, "sub"), filepath.Join(source, "hidden")
	require.NoError(t, os.Rename(sub, hidden))
	require.NoError(t, os.Symlink(t.TempDir(), sub))
	require.ErrorIs(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(5*time.Second)), errWatchBlocked)
	require.NoError(t, os.Remove(sub))
	require.NoError(t, os.Rename(hidden, sub))

	// The settle window started before the subtree was hidden has expired.
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(11*time.Second)))
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/sub/draft.md")
	require.ErrorIs(t, err, store.ErrNotFound, "time hidden behind a symlink must not count toward settling")
	require.NoError(t, scanWatcherAt(t.Context(), watcher, root, t0.Add(21*time.Second)))
	_, err = ing.Store.NodeByPath(t.Context(), "/archive/sub/draft.md")
	require.NoError(t, err)
}
