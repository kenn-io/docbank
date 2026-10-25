package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

const (
	maxWatchHints      = 1024
	maxWatchEntryUnits = 32768
)

// watchScanHints are wake hints, never authority to open a file. The walker
// still checks every admitted entry before any content read.
type watchScanHints struct {
	Dirty     map[string]struct{}
	Reconcile bool
}

func (h watchScanHints) matches(ref string) bool {
	for {
		if _, ok := h.Dirty[ref]; ok {
			return true
		}
		i := strings.LastIndexByte(ref, '/')
		if i < 0 {
			return false
		}
		ref = ref[:i]
	}
}

type watchHintAccumulator struct {
	root      string
	dirty     map[string]struct{}
	reconcile bool
}

// Add accepts only names under the pinned root. A root-level or unmappable
// event asks for a full scan; no event pathname is ever opened directly.
func (a *watchHintAccumulator) Add(name string) {
	if a.reconcile {
		return
	}
	if !filepath.IsAbs(name) || strings.ContainsRune(name, 0) {
		a.dirty = nil
		a.reconcile = true
		return
	}
	ref, err := filepath.Rel(a.root, name)
	if err != nil || ref == "." || ref == ".." || strings.HasPrefix(ref, ".."+string(filepath.Separator)) {
		a.dirty = nil
		a.reconcile = true
		return
	}
	ref = filepath.ToSlash(ref)
	if a.dirty == nil {
		a.dirty = make(map[string]struct{})
	}
	a.dirty[ref] = struct{}{}
	if len(a.dirty) > maxWatchHints {
		a.dirty = nil
		a.reconcile = true
	}
}

func (a *watchHintAccumulator) requestReconcile() {
	a.dirty = nil
	a.reconcile = true
}

func (a *watchHintAccumulator) Drain() watchScanHints {
	hints := watchScanHints{Dirty: a.dirty, Reconcile: a.reconcile}
	a.dirty, a.reconcile = nil, false
	return hints
}

type watchNotificationSource interface {
	BeginScan()
	AddDirectory(ref string, entryCount int) error
	EndScan() error
	Drain() watchScanHints
	Wake() <-chan struct{}
	Errors() <-chan error
	Close() error
}

type watchBackend interface {
	Add(name string) error
	Remove(name string) error
	RemoveBeforeClose(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyBackend struct{ watcher *fsnotify.Watcher }

func (b fsnotifyBackend) Add(name string) error {
	if err := b.watcher.Add(name); err != nil {
		return fmt.Errorf("add filesystem watch %q: %w", name, err)
	}
	return nil
}

func (b fsnotifyBackend) Remove(name string) error {
	if err := b.watcher.Remove(name); err != nil {
		return fmt.Errorf("remove filesystem watch %q: %w", name, err)
	}
	return nil
}

func (b fsnotifyBackend) RemoveBeforeClose(name string) error {
	// Closing inotify releases every watch. Removing both old and new aliases
	// individually is unsafe because fsnotify can retain a stale inode mapping.
	if runtime.GOOS == "linux" {
		return nil
	}
	return b.Remove(name)
}

func (b fsnotifyBackend) Close() error {
	if err := b.watcher.Close(); err != nil {
		return fmt.Errorf("close filesystem watcher: %w", err)
	}
	return nil
}
func (b fsnotifyBackend) Events() <-chan fsnotify.Event { return b.watcher.Events }
func (b fsnotifyBackend) Errors() <-chan error          { return b.watcher.Errors }

// watchNotifications owns one collector and one bounded wake slot. Directory
// registrations are reconciled against the admitted walker traversal.
type watchNotifications struct {
	root          string
	backend       watchBackend
	wake          chan struct{}
	errors        chan error
	stop          chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	closeErr      error
	mu            sync.Mutex
	hints         watchHintAccumulator
	registrations map[string]int
	seen          map[string]int
	scanUnits     int
	units         int
}

func newWatchNotifications(root string, _ exclusions) (*watchNotifications, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create filesystem watcher: %w", err)
	}
	return newWatchNotificationsWithBackend(root, fsnotifyBackend{watcher: watcher}), nil
}

func newWatchNotificationsWithBackend(root string, backend watchBackend) *watchNotifications {
	n := &watchNotifications{
		root: root, backend: backend, wake: make(chan struct{}, 1),
		errors: make(chan error, 1), stop: make(chan struct{}), done: make(chan struct{}),
		registrations: make(map[string]int),
	}
	n.hints.root = root
	go n.collect()
	return n
}

func (n *watchNotifications) collect() {
	defer close(n.done)
	for {
		select {
		case <-n.stop:
			return
		case event, ok := <-n.backend.Events():
			if !ok {
				n.report(errors.New("filesystem notification events closed"))
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			n.mu.Lock()
			n.hints.Add(event.Name)
			n.mu.Unlock()
			select {
			case n.wake <- struct{}{}:
			default:
			}
		case err, ok := <-n.backend.Errors():
			if !ok {
				n.report(errors.New("filesystem notification errors closed"))
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				n.mu.Lock()
				n.hints.requestReconcile()
				n.mu.Unlock()
				select {
				case n.wake <- struct{}{}:
				default:
				}
				continue
			}
			n.report(err)
			return
		}
	}
}

func (n *watchNotifications) report(err error) {
	if err == nil {
		err = errors.New("filesystem notification backend failed")
	}
	select {
	case n.errors <- err:
	default:
	}
}

func (n *watchNotifications) BeginScan() {
	// An incomplete traversal cannot retire live subscriptions. Only its
	// uninstalled intent is discarded when the next scan starts.
	n.seen = make(map[string]int)
	n.scanUnits = 0
}

func (n *watchNotifications) AddDirectory(ref string, entryCount int) error {
	if entryCount < 0 {
		return fmt.Errorf("invalid directory entry count %d", entryCount)
	}
	previous := n.registrations[ref]
	desiredUnits := n.units - previous
	if n.seen != nil {
		desiredUnits = n.scanUnits - n.seen[ref]
	}
	if entryCount >= maxWatchEntryUnits-desiredUnits {
		err := fmt.Errorf("filesystem notification budget exceeds %d entry units", maxWatchEntryUnits)
		_ = n.Close()
		return err
	}
	cost := 1 + entryCount
	if n.seen != nil {
		n.seen[ref] = cost
		n.scanUnits = desiredUnits + cost
	}
	if n.units-previous+cost > maxWatchEntryUnits {
		// Keep old registrations charged until a complete scan can remove
		// unseen paths. The desired cost above is bounded independently.
		return nil
	}
	// Add is idempotent for a live watch and re-enrolls a directory whose
	// prior inode was deleted and replaced at the same relative path.
	name := filepath.Join(n.root, filepath.FromSlash(ref))
	// kqueue can partially enroll a directory before Add fails on a child.
	// Keep the attempted path so Close can remove those registrations too.
	n.registrations[ref] = cost
	n.units += cost - previous
	if err := n.backend.Add(name); err != nil {
		_ = n.Close()
		return fmt.Errorf("subscribing to %q: %w", ref, err)
	}
	return nil
}

func (n *watchNotifications) EndScan() error {
	retired := false
	for ref, cost := range n.registrations {
		if _, ok := n.seen[ref]; ok {
			continue
		}
		name := filepath.Join(n.root, filepath.FromSlash(ref))
		if err := n.backend.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			_ = n.Close()
			return fmt.Errorf("removing subscription %q: %w", ref, err)
		}
		delete(n.registrations, ref)
		n.units -= cost
		retired = true
	}
	// Removing an old pathname can remove an inode watch shared with a renamed
	// path. Refresh admitted paths after retirement even when their costs match.
	// Retiring unseen paths makes room within the checked scan budget.
	for ref, cost := range n.seen {
		if retired || n.registrations[ref] != cost {
			if err := n.AddDirectory(ref, cost-1); err != nil {
				return err
			}
		}
	}
	n.seen = nil
	n.scanUnits = 0
	return nil
}

func (n *watchNotifications) Drain() watchScanHints {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hints.Drain()
}
func (n *watchNotifications) Wake() <-chan struct{} { return n.wake }
func (n *watchNotifications) Errors() <-chan error  { return n.errors }

func (n *watchNotifications) Close() error {
	n.closeOnce.Do(func() {
		close(n.stop)
		// fsnotify's kqueue backend marks itself closed before its own Remove
		// calls, so release enrolled paths while Remove can still close them.
		for ref := range n.registrations {
			name := filepath.Join(n.root, filepath.FromSlash(ref))
			if err := n.backend.RemoveBeforeClose(name); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				n.closeErr = errors.Join(n.closeErr, fmt.Errorf("removing subscription %q: %w", name, err))
			}
		}
		n.closeErr = errors.Join(n.closeErr, n.backend.Close())
	})
	<-n.done
	n.registrations = make(map[string]int)
	n.seen = nil
	n.scanUnits = 0
	n.units = 0
	return n.closeErr
}
