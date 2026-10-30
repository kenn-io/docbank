package ingest

import (
	"context"
	"io/fs"
	"strings"

	"errors"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/config"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

func TestWatchNotificationsBoundAndCoalesce(t *testing.T) {
	root := t.TempDir()
	var pending watchHintAccumulator
	pending.root = root
	for range 100 {
		pending.Add(filepath.Join(root, "nested", "one.txt"))
	}
	hints := pending.Drain()
	require.False(t, hints.Reconcile)
	require.Equal(t, map[string]struct{}{"nested/one.txt": {}}, hints.Dirty)
	require.Empty(t, pending.Drain().Dirty)
	pending.Add(filepath.Join(root, "nested"))
	hints = pending.Drain()
	require.True(t, hints.matches("nested/child.txt"))
	require.False(t, hints.matches("elsewhere.txt"))
	for i := range 1025 {
		pending.Add(filepath.Join(root, fmt.Sprintf("%d.txt", i)))
	}
	pending.Add(filepath.Join(root, "another.txt"))
	hints = pending.Drain()
	require.True(t, hints.Reconcile)
	require.Empty(t, hints.Dirty)
	backend := newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, backend)
	require.NoError(t, n.Close())
	select {
	case <-n.done:
	default:
		t.Fatal("Close did not join collector")
	}
}

func FuzzWatchNotificationBatch(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{0, 0, 0, 0, 0})
	f.Add([]byte{3, 2, 1, 5, 6, 0})
	f.Add(make([]byte, 1025))
	f.Fuzz(func(t *testing.T, instructions []byte) {
		if len(instructions) > 4096 {
			t.Skip()
		}
		root := filepath.Join(t.TempDir(), "source")
		var pending watchHintAccumulator
		pending.root = root
		valid := map[string]struct{}{}
		for index, op := range instructions {
			switch op % 7 {
			case 0:
				ref := fmt.Sprintf("dir/file-%d.txt", index)
				pending.Add(filepath.Join(root, filepath.FromSlash(ref)))
				valid[ref] = struct{}{}
			case 1:
				pending.Add(filepath.Join(root, "dir", "file.txt"))
				valid["dir/file.txt"] = struct{}{}
			case 2:
				pending.Add(filepath.Join(root, "dir"))
				valid["dir"] = struct{}{}
			case 3:
				pending.Add(filepath.Join(root, "..", "outside"))
			case 4:
				pending.Add(filepath.Join(root, "dir", "..", "other.txt"))
				valid["other.txt"] = struct{}{}
			case 5:
				pending.Add("../relative")
			case 6:
				hints := pending.Drain()
				require.LessOrEqual(t, len(hints.Dirty), 1024)
				for ref := range valid {
					require.True(t, hints.Reconcile || hints.matches(ref), ref)
				}
				for ref := range hints.Dirty {
					require.NotContains(t, ref, "..")
					require.False(t, filepath.IsAbs(ref))
				}
				clear(valid)
			}
		}
		hints := pending.Drain()
		require.LessOrEqual(t, len(hints.Dirty), 1024)
		for ref := range valid {
			require.True(t, hints.Reconcile || hints.matches(ref), ref)
		}
	})
}

type fakeWatchNotificationSource struct {
	mu       sync.Mutex
	pending  watchHintAccumulator
	wake     chan struct{}
	errs     chan error
	closed   chan struct{}
	once     sync.Once
	endScans int
}

func newFakeWatchNotificationSource(root string) *fakeWatchNotificationSource {
	return &fakeWatchNotificationSource{pending: watchHintAccumulator{root: root}, wake: make(chan struct{}, 1), errs: make(chan error, 1), closed: make(chan struct{})}
}
func (f *fakeWatchNotificationSource) BeginScan()                     {}
func (f *fakeWatchNotificationSource) AddDirectory(string, int) error { return nil }
func (f *fakeWatchNotificationSource) EndScan() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endScans++
	return nil
}
func (f *fakeWatchNotificationSource) Drain() watchScanHints {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending.Drain()
}
func (f *fakeWatchNotificationSource) Wake() <-chan struct{} { return f.wake }
func (f *fakeWatchNotificationSource) Errors() <-chan error  { return f.errs }
func (f *fakeWatchNotificationSource) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}
func (f *fakeWatchNotificationSource) Emit(path string) {
	f.mu.Lock()
	f.pending.Add(path)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func TestWatchEventRunnerCapturesObservableSaves(t *testing.T) {
	ing := newTestIngester(t)
	source := writeTree(t, map[string]string{"notes.txt": "first"})
	notifier := newFakeWatchNotificationSource(source)
	commits := make(chan struct{}, 8)
	watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		SettleTime: config.Duration(30 * time.Second), ScanInterval: config.Duration(time.Hour),
	}, func(fn func() error) error {
		err := fn()
		if err == nil {
			commits <- struct{}{}
		}
		return err
	}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	watcher.newNotifications = func(string, exclusions) (watchNotificationSource, error) { return notifier, nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	defer func() { cancel(); require.ErrorIs(t, <-done, context.Canceled); <-notifier.closed }()
	awaitVersions := func(want int) {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			node, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
			if err == nil {
				_, count, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
				require.NoError(t, err)
				if count == want {
					return
				}
				require.Less(t, count, want)
			}
			select {
			case <-commits:
			case <-deadline.C:
				stacktest.Dump(t)
				t.Fatalf("timed out waiting for %d versions", want)
			}
		}
	}
	awaitVersions(1)
	pathname := filepath.Join(source, "notes.txt")
	require.NoError(t, os.WriteFile(pathname, []byte("second"), 0600))
	notifier.Emit(pathname)
	awaitVersions(2)
	replacement := filepath.Join(source, "replacement.tmp")
	require.NoError(t, os.WriteFile(replacement, []byte("third"), 0600))
	require.NoError(t, os.Rename(replacement, pathname))
	notifier.Emit(pathname)
	awaitVersions(3)
	node, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
	require.NoError(t, err)
	versions, total, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, versions, 3)
}

func TestFolderWatchEventRunnerUsesSharedScanner(t *testing.T) {
	source := writeTree(t, map[string]string{"notes.txt": "first"})
	notifier := newFakeWatchNotificationSource(source)
	contents := make(chan string, 8)
	scanner, err := NewFolderScanner(config.WatchConfig{
		Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
		SettleTime: config.Duration(30 * time.Second), ScanInterval: config.Duration(time.Hour),
	}, func(ctx context.Context, file FolderFile) error {
		data, err := io.ReadAll(file.File)
		if err != nil {
			return err
		}
		if _, err := file.File.Seek(0, 0); err != nil {
			return err
		}
		if _, err := file.Digest(ctx); err != nil {
			return err
		}
		contents <- string(data)
		return nil
	}, failOnReject(t))
	require.NoError(t, err)
	processed := make(chan struct{}, 8)
	process := scanner.watcher.process
	scanner.watcher.process = func(ctx context.Context, ref string, observation *watchObservation, open localFileOpener) error {
		err := process(ctx, ref, observation, open)
		if err == nil {
			processed <- struct{}{}
		}
		return err
	}
	scanner.watcher.newNotifications = func(string, exclusions) (watchNotificationSource, error) { return notifier, nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scanner.Run(ctx) }()
	defer func() { cancel(); require.ErrorIs(t, <-done, context.Canceled) }()
	await := func(want string) {
		t.Helper()
		select {
		case got := <-contents:
			require.Equal(t, want, got)
		case <-time.After(10 * time.Second):
			stacktest.Dump(t)
			t.Fatalf("timed out waiting for %q", want)
		}
		// The scanner rechecks and closes the source after the callback returns.
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out finishing processing %q", want)
		}
	}
	await("first")
	pathname := filepath.Join(source, "notes.txt")
	require.NoError(t, os.WriteFile(pathname, []byte("second"), 0600))
	notifier.Emit(pathname)
	await("second")
	replacement := filepath.Join(source, "replacement.tmp")
	require.NoError(t, os.WriteFile(replacement, []byte("third"), 0600))
	require.NoError(t, os.Rename(replacement, pathname))
	notifier.Emit(pathname)
	await("third")
	select {
	case got := <-contents:
		t.Fatalf("unexpected duplicate: %q", got)
	default:
	}
}

type fakeWatchBackend struct {
	events    chan fsnotify.Event
	errs      chan error
	closed    chan struct{}
	closeOnce sync.Once
	addErr    func(string) error
	removeErr error
	closeErr  error
	onRemove  func(string)
	onClose   func()
	added     []string
	removed   []string
}

func newFakeWatchBackend() *fakeWatchBackend {
	return &fakeWatchBackend{events: make(chan fsnotify.Event, 8), errs: make(chan error, 1), closed: make(chan struct{})}
}
func (b *fakeWatchBackend) Add(name string) error {
	b.added = append(b.added, name)
	if b.addErr != nil {
		return b.addErr(name)
	}
	return nil
}
func (b *fakeWatchBackend) Remove(name string) error {
	b.removed = append(b.removed, name)
	if b.onRemove != nil {
		b.onRemove(name)
	}
	return b.removeErr
}
func (b *fakeWatchBackend) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	b.closeOnce.Do(func() { close(b.closed) })
	return b.closeErr
}
func (b *fakeWatchBackend) Events() <-chan fsnotify.Event { return b.events }
func (b *fakeWatchBackend) Errors() <-chan error          { return b.errs }

func TestWatchNotificationLowCostRenameRefreshesDescendantSubscription(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	// Linux inotify aliases the old and renamed pathnames to one inode watch.
	// Each descendant also keeps an aliased watch across the rename.
	identities := map[string]string{
		root:                                    "root",
		filepath.Join(root, "before"):           "parent",
		filepath.Join(root, "after"):            "parent",
		filepath.Join(root, "before", "nested"): "child",
		filepath.Join(root, "after", "nested"):  "child",
	}
	live := make(map[string]bool)
	retiredChild, refreshedChild := false, false
	backend.addErr = func(name string) error {
		require.Contains(t, identities, name)
		live[identities[name]] = true
		if name == filepath.Join(root, "after", "nested") && retiredChild {
			refreshedChild = true
		}
		return nil
	}
	backend.onRemove = func(name string) {
		delete(live, identities[name])
		if name == filepath.Join(root, "before", "nested") {
			retiredChild = true
		}
	}
	n := newWatchNotificationsWithBackend(root, backend)
	t.Cleanup(func() { require.NoError(t, n.Close()) })
	n.BeginScan()
	for _, ref := range []string{"", "before", "before/nested"} {
		require.NoError(t, n.AddDirectory(ref, 1))
	}
	require.NoError(t, n.EndScan())
	n.BeginScan()
	for _, ref := range []string{"", "after", "after/nested"} {
		require.NoError(t, n.AddDirectory(ref, 1))
	}
	require.Contains(t, backend.added, filepath.Join(root, "after", "nested"), "exercise early enrollment below the budget")
	require.NoError(t, n.EndScan())
	require.Equal(t, map[string]bool{"root": true, "parent": true, "child": true}, live,
		"retiring old aliases must leave fresh live subscriptions for the renamed subtree")
	require.True(t, refreshedChild, "Add(new) must follow Remove(old) for the descendant watch")
	require.Equal(t, map[string]int{"": 2, "after": 2, "after/nested": 2}, n.registrations)
	require.Equal(t, 6, n.units)
}

func TestWatchNotificationCloseReleasesThroughBackendClose(t *testing.T) {
	for _, mode := range []string{"pending_events", "partial_add", "budget", "close_error"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			backend := newFakeWatchBackend()
			// Windows Remove waits for a reader blocked on undelivered events.
			blocked := make(chan struct{})
			t.Cleanup(func() { close(blocked) })
			backend.onRemove = func(string) { <-blocked }
			closeCalls := 0
			backend.onClose = func() { closeCalls++ }
			n := newWatchNotificationsWithBackend(root, backend)
			n.BeginScan()
			require.NoError(t, n.AddDirectory("", 1))
			closeFailure := errors.New("synthetic close failure")
			switch mode {
			case "pending_events":
				require.NoError(t, n.AddDirectory("nested", 0))
				for range cap(backend.events) {
					backend.events <- fsnotify.Event{Name: filepath.Join(root, "nested", "busy.txt"), Op: fsnotify.Write}
				}
			case "partial_add":
				addFailure := errors.New("synthetic partial enrollment failure")
				backend.addErr = func(string) error { return addFailure }
				require.ErrorIs(t, n.AddDirectory("partial", 0), addFailure)
			case "budget":
				require.ErrorContains(t, n.AddDirectory("oversized", maxWatchEntryUnits), "budget exceeds")
			case "close_error":
				backend.closeErr = closeFailure
			}
			closed := make(chan error, 1)
			go func() {
				first := n.Close()
				closed <- errors.Join(first, n.Close())
			}()
			select {
			case err := <-closed:
				if mode == "close_error" {
					require.ErrorIs(t, err, closeFailure)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(10 * time.Second):
				stacktest.Dump(t)
				t.Fatal("Close blocked before releasing the backend")
			}
			require.Equal(t, 1, closeCalls)
			require.Empty(t, backend.removed, "backend Close releases every watch")
			require.Empty(t, n.registrations)
			require.Empty(t, n.seen)
			require.Zero(t, n.units)
			require.Zero(t, n.scanUnits)
		})
	}
}

func TestWatchNotificationRegistrationBudgetAndLifecycle(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, backend)
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 32750))
	require.NoError(t, n.AddDirectory("nested", 16))
	require.NoError(t, n.EndScan())
	require.Equal(t, 32768, n.units)
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 32750))
	require.NoError(t, n.EndScan())
	require.Equal(t, 32751, n.units, "removing subtree releases its entry units")
	require.Equal(t, []string{filepath.Join(root, "nested")}, backend.removed)
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 32760))
	require.NoError(t, n.EndScan())
	require.Equal(t, 32761, n.units, "growth in an existing registration updates cost")
	n.BeginScan()
	err := n.AddDirectory("", 32768)
	require.ErrorContains(t, err, "32768")
	require.Equal(t, 0, n.units)
	select {
	case <-backend.closed:
	default:
		t.Fatal("budget failure must close native subscriptions")
	}
	require.NoError(t, n.Close())
	select {
	case <-n.done:
	default:
		t.Fatal("Close must join collector")
	}
}

func TestWatchNotificationRenamedDirectoryKeepsBudget(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, backend)
	t.Cleanup(func() { require.NoError(t, n.Close()) })
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("before", 20000))
	require.NoError(t, n.EndScan())
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("after", 20000))
	require.NotContains(t, backend.added, filepath.Join(root, "after"), "defer until retired watches release capacity")
	backend.addErr = func(name string) error {
		require.Contains(t, backend.removed, filepath.Join(root, "before"), "retire before deferred Add")
		return nil
	}
	require.NoError(t, n.EndScan())
	require.Equal(t, map[string]int{"": 2, "after": 20001}, n.registrations)
	require.Equal(t, 20003, n.units)
	select {
	case <-backend.closed:
		t.Fatal("renaming an admitted tree disabled notifications")
	default:
	}
}

func TestWatchNotificationDeferredEnrollmentFailureCloses(t *testing.T) {
	for _, operation := range []string{"add", "remove"} {
		t.Run(operation, func(t *testing.T) {
			backend := newFakeWatchBackend()
			n := newWatchNotificationsWithBackend(t.TempDir(), backend)
			t.Cleanup(func() { require.NoError(t, n.Close()) })
			n.BeginScan()
			require.NoError(t, n.AddDirectory("", 1))
			require.NoError(t, n.AddDirectory("before", 20000))
			require.NoError(t, n.EndScan())
			n.BeginScan()
			require.NoError(t, n.AddDirectory("", 1))
			require.NoError(t, n.AddDirectory("after", 20000))
			failure := errors.New("synthetic backend failure")
			if operation == "add" {
				backend.addErr = func(string) error { return failure }
			} else {
				backend.removeErr = failure
			}
			require.ErrorIs(t, n.EndScan(), failure)
			require.Zero(t, n.units)
			require.Empty(t, n.registrations)
			select {
			case <-backend.closed:
			default:
				t.Fatal("failed reconciliation kept subscriptions open")
			}
		})
	}
}

func FuzzWatchNotificationRegistrationScans(f *testing.F) {
	f.Add(20000, 0, 20000, 0, true, false)
	f.Add(10000, 20000, 30000, 0, false, false)
	f.Add(32763, 0, 0, 32763, false, true)
	f.Add(32764, 0, 0, 0, true, true)
	f.Add(0, 0, int(^uint(0)>>1), 0, false, false)
	// Fuzz values are counts; clamp negative generated inputs to zero so this
	// target exercises only values reachable through production callers.
	f.Fuzz(func(t *testing.T, oldLeft, oldRight, nextLeft, nextRight int, rename, reverse bool) {
		oldLeft, oldRight = max(oldLeft, 0), max(oldRight, 0)
		nextLeft, nextRight = max(nextLeft, 0), max(nextRight, 0)
		root := t.TempDir()
		backend := newFakeWatchBackend()
		n := newWatchNotificationsWithBackend(root, backend)
		t.Cleanup(func() { require.NoError(t, n.Close()) })
		active, desired := map[string]int{}, map[string]int{}
		backend.addErr = func(name string) error {
			active[name] = desired[name]
			require.Positive(t, active[name])
			units := 0
			for _, cost := range active {
				units += cost
			}
			require.LessOrEqual(t, units, maxWatchEntryUnits, "actual cost at backend Add")
			return nil
		}
		backend.onRemove = func(name string) { delete(active, name) }
		for scan := range 2 {
			left, right, ref := oldLeft, oldRight, "left"
			if scan == 1 {
				left, right = nextLeft, nextRight
				if rename {
					ref = "renamed"
				}
			}
			desired = map[string]int{root: 3, filepath.Join(root, ref): left + 1, filepath.Join(root, "right"): right + 1}
			n.BeginScan()
			require.NoError(t, n.AddDirectory("", 2))
			refs, counts := []string{ref, "right"}, []int{left, right}
			if reverse {
				refs[0], refs[1], counts[0], counts[1] = refs[1], refs[0], counts[1], counts[0]
			}
			scannedUnits := 3
			for i, name := range refs {
				err := n.AddDirectory(name, counts[i])
				if counts[i] >= maxWatchEntryUnits || scannedUnits+counts[i]+1 > maxWatchEntryUnits {
					require.ErrorContains(t, err, "budget exceeds")
					require.Zero(t, n.units)
					select {
					case <-backend.closed:
					default:
						t.Fatal("oversized current inventory kept subscriptions open")
					}
					return
				}
				require.NoError(t, err)
				scannedUnits += counts[i] + 1
			}
			require.NoError(t, n.EndScan())
			require.Equal(t, desired, active)
			require.Equal(t, left+right+5, n.units)
		}
	})
}

func TestWatchNotificationBackendFailureClosesPartialSubscriptions(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	backend.addErr = func(name string) error {
		if strings.HasSuffix(name, "nested") {
			return fs.ErrNotExist
		}
		return nil
	}
	n := newWatchNotificationsWithBackend(root, backend)
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 2))
	require.ErrorIs(t, n.AddDirectory("nested", 1), fs.ErrNotExist)
	select {
	case <-backend.closed:
	default:
		t.Fatal("partial adapter must be closed")
	}
	require.NoError(t, n.Close())
	require.Zero(t, n.units)
	require.Len(t, backend.added, 2)

	live := newFakeWatchBackend()
	n = newWatchNotificationsWithBackend(root, live)
	live.errs <- errors.New("backend stopped")
	select {
	case err := <-n.Errors():
		require.ErrorContains(t, err, "backend stopped")
	case <-time.After(10 * time.Second):
		stacktest.Dump(t)
		t.Fatal("runtime backend failure not reported")
	}
	require.NoError(t, n.Close())
	select {
	case <-n.done:
	default:
		t.Fatal("collector did not stop")
	}
}

func TestPromptWatchNotificationFailureFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		vanished bool
	}{
		{name: "runtime failure"},
		{name: "vanished during add", vanished: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ing := newTestIngester(t)
			source := writeTree(t, map[string]string{"notes.txt": "first", "nested/other.txt": "content"})
			commits := make(chan struct{}, 8)
			var logs strings.Builder
			watcher, err := NewWatcher(ing, t.TempDir(), config.WatchConfig{
				Name: "notes", Source: source, Destination: "/archive", CaptureOnChange: true,
				SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(100 * time.Millisecond),
			}, func(fn func() error) error {
				err := fn()
				if err == nil {
					select {
					case commits <- struct{}{}:
					default:
					}
				}
				return err
			}, slog.New(slog.NewTextHandler(&logs, nil)))
			require.NoError(t, err)
			firstBackend := newFakeWatchBackend()
			if tc.vanished {
				firstBackend.addErr = func(name string) error {
					if strings.HasSuffix(name, "nested") {
						return fs.ErrNotExist
					}
					return nil
				}
			}
			factories := make(chan int, 8)
			attempts := 0
			watcher.newNotifications = func(root string, _ exclusions) (watchNotificationSource, error) {
				attempts++
				factories <- attempts
				if attempts == 1 {
					return newWatchNotificationsWithBackend(root, firstBackend), nil
				}
				return newWatchNotificationsWithBackend(root, newFakeWatchBackend()), nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- watcher.Run(ctx) }()
			joined := false
			defer func() {
				if !joined {
					cancel()
					require.ErrorIs(t, <-done, context.Canceled)
				}
			}()
			initialDeadline := time.NewTimer(10 * time.Second)
			defer initialDeadline.Stop()
			for {
				node, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
				if err == nil {
					_, count, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
					require.NoError(t, err)
					if count == 1 {
						break
					}
				}
				select {
				case <-commits:
				case <-initialDeadline.C:
					stacktest.Dump(t)
					t.Fatal("initial source not committed")
				}
			}
			if !tc.vanished {
				firstBackend.errs <- errors.New("synthetic backend failure")
			}
			select {
			case <-firstBackend.closed:
			case <-time.After(10 * time.Second):
				stacktest.Dump(t)
				t.Fatal("failed backend still open")
			}
			require.NoError(t, os.WriteFile(filepath.Join(source, "notes.txt"), []byte("second"), 0600))
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			for {
				node, err := ing.Store.NodeByPath(t.Context(), "/archive/notes.txt")
				if err == nil {
					_, count, err := ing.Store.ContentVersions(t.Context(), node.ID, 10, 0)
					require.NoError(t, err)
					if count == 2 {
						break
					}
				}
				select {
				case <-commits:
				case <-deadline.C:
					stacktest.Dump(t)
					t.Fatal("polling did not capture later change")
				}
			}
			if tc.vanished {
				select {
				case count := <-factories:
					require.Equal(t, 1, count)
				default:
					t.Fatal("initial enrollment missing")
				}
				select {
				case count := <-factories:
					require.Equal(t, 2, count)
				case <-time.After(10 * time.Second):
					stacktest.Dump(t)
					t.Fatal("no periodic re-enrollment")
				}
			} else {
				select {
				case count := <-factories:
					require.Equal(t, 1, count)
				default:
					t.Fatal("initial enrollment missing")
				}
				select {
				case count := <-factories:
					t.Fatalf("runtime failure unexpectedly retried: %d", count)
				default:
				}
			}
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			joined = true
			require.Contains(t, logs.String(), "polling continues")
			if tc.vanished {
				require.Contains(t, logs.String(), "notifications restored")
			}
		})
	}
}

func TestWatchNotificationMissingNativeWatchReleasesBudget(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, backend)
	defer func() { require.NoError(t, n.Close()) }()
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("nested", 0))
	require.NoError(t, n.EndScan())
	require.Equal(t, 3, n.units)
	backend.removeErr = fsnotify.ErrNonExistentWatch
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 0))
	require.NoError(t, n.EndScan())
	require.Equal(t, 1, n.units)
	require.Equal(t, []string{filepath.Join(root, "nested")}, backend.removed)
	select {
	case <-backend.closed:
		t.Fatal("ordinary deletion disabled all notifications")
	default:
	}
}

func TestWatchNotificationRecreatedDirectoryIsSubscribed(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, backend)
	defer func() { require.NoError(t, n.Close()) }()
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("nested", 0))
	require.NoError(t, n.EndScan())
	backend.events <- fsnotify.Event{Name: filepath.Join(root, "nested"), Op: fsnotify.Remove}
	select {
	case <-n.Wake():
	case <-time.After(3 * time.Second):
		stacktest.Dump(t)
		t.Fatal("no deletion hint")
	}
	_ = n.Drain()
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("nested", 0))
	require.NoError(t, n.EndScan())
	count := 0
	for _, name := range backend.added {
		if name == filepath.Join(root, "nested") {
			count++
		}
	}
	require.Equal(t, 2, count, "a recreated directory needs a fresh native Add")
}

func TestWatchNotificationOverflowWakesReconciliationAndContinues(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, backend)
	defer func() { require.NoError(t, n.Close()) }()
	backend.events <- fsnotify.Event{Name: filepath.Join(root, "old.txt"), Op: fsnotify.Write}
	select {
	case <-n.Wake():
	case <-time.After(3 * time.Second):
		stacktest.Dump(t)
		t.Fatal("no initial hint")
	}
	backend.errs <- fsnotify.ErrEventOverflow
	select {
	case <-n.Wake():
	case <-time.After(3 * time.Second):
		stacktest.Dump(t)
		t.Fatal("overflow did not wake")
	}
	hints := n.Drain()
	require.True(t, hints.Reconcile)
	require.Empty(t, hints.Dirty, "overflow clears incomplete hints")
	select {
	case err := <-n.Errors():
		t.Fatalf("overflow stopped collector: %v", err)
	default:
	}
	backend.events <- fsnotify.Event{Name: filepath.Join(root, "later.txt"), Op: fsnotify.Write}
	select {
	case <-n.Wake():
	case <-time.After(3 * time.Second):
		stacktest.Dump(t)
		t.Fatal("collector did not continue")
	}
	hints = n.Drain()
	require.False(t, hints.Reconcile)
	require.True(t, hints.matches("later.txt"))
}

func TestWatchNotificationDroppedDirectoryRecyclesKqueueBackend(t *testing.T) {
	root := t.TempDir()
	first, second := newFakeWatchBackend(), newFakeWatchBackend()
	n := newWatchNotificationsWithBackend(root, first)
	defer func() { require.NoError(t, n.Close()) }()
	n.replaceBackend = func() (watchBackend, error) { return second, nil }
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("before", 3))
	require.NoError(t, n.EndScan())

	// kqueue already dropped the renamed directory's own watch, keeping its
	// per-file child watches open until the backend closes.
	first.removeErr = fsnotify.ErrNonExistentWatch
	n.BeginScan()
	require.NoError(t, n.AddDirectory("", 1))
	require.NoError(t, n.AddDirectory("after", 3))
	require.NoError(t, n.EndScan())
	select {
	case <-first.closed:
	default:
		t.Fatal("the backend holding orphaned child watches stayed open")
	}
	require.ElementsMatch(t, []string{root, filepath.Join(root, "after")}, second.added)
	require.Equal(t, map[string]int{"": 2, "after": 4}, n.registrations)
	require.Equal(t, 6, n.units)
	select {
	case <-n.Wake():
	case <-time.After(3 * time.Second):
		stacktest.Dump(t)
		t.Fatal("events between backends need a reconciliation")
	}
	require.True(t, n.Drain().Reconcile)

	second.events <- fsnotify.Event{Name: filepath.Join(root, "after", "new.txt"), Op: fsnotify.Create}
	select {
	case <-n.Wake():
	case <-time.After(3 * time.Second):
		stacktest.Dump(t)
		t.Fatal("the replacement backend is not collected")
	}
	require.True(t, n.Drain().matches("after/new.txt"))
}
