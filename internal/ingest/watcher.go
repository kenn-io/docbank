package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

// WatchOutcome describes what one stable watched-file observation changed.
type WatchOutcome string

const (
	WatchAdded   WatchOutcome = "added"
	WatchUpdated WatchOutcome = "updated"
	WatchSkipped WatchOutcome = "skipped"
)

// WatchResult binds one watched source to the stable node it created, updated,
// or confirmed unchanged.
type WatchResult struct {
	Node    store.Node
	Outcome WatchOutcome
}

func (ing *Ingester) ingestWatchedFile(
	ctx context.Context, watchName, destination, sourceRef string,
	expected localFileFingerprint, open localFileOpener,
) (result WatchResult, retErr error) {
	sourceRef = filepath.ToSlash(sourceRef)
	if sourceRef == "." || sourceRef == "" || path.IsAbs(sourceRef) ||
		sourceRef == ".." || strings.HasPrefix(sourceRef, "../") ||
		path.Clean(sourceRef) != sourceRef {
		return result, fmt.Errorf("invalid watched source reference %q", sourceRef)
	}
	if err := validateSourcePath(sourceRef); err != nil {
		return result, err
	}
	if err := validateVirtualDestination(destination); err != nil {
		return result, err
	}

	content, err := ing.readLocalFileWith(ctx, open, sourceRef, nil, &expected)
	if err != nil {
		return result, err
	}
	defer func() {
		retErr = mutationCleanupResult(retErr, ing.cleanupLoose(content.hash))
	}()
	existing, _, changed, err := ing.Store.SyncWatchedContent(
		ctx, watchName, sourceRef,
		content.hash, content.size, content.mimeType, content.physical,
	)
	switch {
	case err == nil:
		if !changed {
			result.Node, result.Outcome = existing, WatchSkipped
			return result, nil
		}
		result.Node = existing
		result.Outcome = WatchUpdated
		return result, nil
	case !errors.Is(err, store.ErrNotFound):
		return result, err
	}

	parentPath := path.Join(destination, path.Dir(sourceRef))
	parent, err := ing.Store.MkdirAll(ctx, parentPath)
	if err != nil {
		return result, fmt.Errorf("creating watched destination %q: %w", parentPath, err)
	}
	run, err := ing.Store.BeginIngest(ctx, "watch", watchName)
	if err != nil {
		return result, err
	}
	result.Node, err = ing.Store.IngestFileExact(
		ctx, run, parent.ID, path.Base(sourceRef), content.hash, content.size,
		content.mimeType, sourceRef, content.mtime, content.physical,
	)
	if err != nil {
		return result, fmt.Errorf("recording watched source %q: %w", sourceRef, err)
	}
	result.Outcome = WatchAdded
	return result, nil
}

func validateVirtualDestination(destination string) error {
	if !strings.HasPrefix(destination, "/") {
		return fmt.Errorf("watched destination %q must be an absolute virtual path", destination)
	}
	for segment := range strings.SplitSeq(destination, "/") {
		if segment == "" {
			continue
		}
		if _, err := store.NormalizeName(segment); err != nil {
			return fmt.Errorf("watched destination %q: %w", destination, err)
		}
	}
	return nil
}

type watchFingerprint = localFileFingerprint

type watchObservation struct {
	fingerprint         watchFingerprint
	stableSince         time.Time
	processed           bool
	digest              string
	accepted            *watchAcceptedSource
	settlingInterrupted bool
}

type watchRoot struct {
	path     string
	root     *os.Root
	identity fs.FileInfo
}

func (root *watchRoot) Close() error { return root.root.Close() }

func (root *watchRoot) verify() error {
	info, err := os.Lstat(root.path)
	if err != nil {
		return fmt.Errorf("checking watched root %q: %w",
			root.path, errors.Join(ErrSourceChanged, err))
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() ||
		!os.SameFile(info, root.identity) {
		return fmt.Errorf("watched root %q was replaced: %w", root.path, ErrSourceChanged)
	}
	return nil
}

func openWatchDirectory(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&fs.ModeSymlink != 0 || !before.IsDir() {
		return nil, ErrSourceChanged
	}
	childRoot, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = childRoot.Close()
		}
	}()
	opened, err := childRoot.Stat(".")
	if err != nil {
		return nil, err
	}
	after, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if after.Mode()&fs.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(after, opened) {
		return nil, ErrSourceChanged
	}
	success = true
	return childRoot, nil
}

type watchSource struct {
	root *watchRoot
	ref  string
}

func (source watchSource) open() (*os.File, error) {
	if err := source.root.verify(); err != nil {
		return nil, err
	}
	f, err := openWatchFile(source.root.root, source.ref)
	if err != nil {
		return nil, err
	}
	if err := source.root.verify(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openWatchFile(root *os.Root, ref string) (*os.File, error) {
	parts := strings.Split(ref, "/")
	dir := root
	owned := false
	defer func() {
		if owned {
			_ = dir.Close()
		}
	}()
	for _, component := range parts[:len(parts)-1] {
		child, err := openWatchDirectory(dir, component)
		if err != nil {
			return nil, err
		}
		if owned {
			_ = dir.Close()
		}
		dir, owned = child, true
	}
	name := parts[len(parts)-1]
	before, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&fs.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, ErrSourceChanged
	}
	f, err := openWatchLeaf(dir, name)
	if err != nil {
		return nil, err
	}
	opened, statErr := f.Stat()
	after, pathErr := dir.Lstat(name)
	if statErr != nil || pathErr != nil || after.Mode()&fs.ModeSymlink != 0 ||
		!after.Mode().IsRegular() || !os.SameFile(before, opened) ||
		!os.SameFile(after, opened) {
		_ = f.Close()
		return nil, errors.Join(ErrSourceChanged, statErr, pathErr)
	}
	return f, nil
}

func (source watchSource) observe(withChange bool) (watchFileState, error) {
	f, err := source.open()
	if err != nil {
		return watchFileState{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return watchFileState{}, err
	}
	mount, identity, err := watchMetadataForFile(f, info)
	if err != nil {
		return watchFileState{}, err
	}
	fingerprint := fingerprintOpenFile(f, info, withChange)
	return watchFileState{fingerprint: fingerprint, mount: mount, identity: identity, info: info}, nil
}

func watchMountForRoot(root *os.Root) (watchMount, error) {
	var noMount watchMount
	f, err := root.Open(".")
	if err != nil {
		return noMount, err
	}
	defer func() { _ = f.Close() }()
	return watchMountForFile(f)
}

// Watcher scans one configured local inbox. Settled observations are in memory,
// so a restart begins a new settle window. Prompt mode skips that delay.
type Watcher struct {
	process          func(context.Context, string, *watchObservation, localFileOpener) error
	reject           func(context.Context, string, error) error
	excluded         func(context.Context, string) error
	sourceChanged    func(context.Context, string, error) error
	beforeFile       func(context.Context, string) error
	rejected         map[string]struct{}
	immediate        bool
	ing              *Ingester
	vaultRoot        string
	vaultDirs        []fs.FileInfo
	config           config.WatchConfig
	excludes         exclusions
	mutate           func(func() error) error
	logger           *slog.Logger
	now              func() time.Time
	beforeDescend    func(string)
	beforeObserve    func(string)
	observeFile      func(watchSource, bool) (watchFileState, error)
	beforeRebind     func(string)
	sourceMount      watchMount
	observations     map[string]watchObservation
	newNotifications func(string, exclusions) (watchNotificationSource, error)
	notifications    watchNotificationSource
	notificationErr  error
}

// NewWatcher validates the non-filesystem portion of one watched inbox.
func NewWatcher(
	ing *Ingester, vaultRoot string, cfg config.WatchConfig,
	mutate func(func() error) error, logger *slog.Logger,
) (*Watcher, error) {
	if ing == nil || ing.Store == nil || ing.Blobs == nil {
		return nil, errors.New("watched inbox requires an ingester")
	}
	if mutate == nil {
		return nil, errors.New("watched inbox requires the daemon mutation gate")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := validateVirtualDestination(cfg.Destination); err != nil {
		return nil, err
	}
	excludes, err := compileExclusions(cfg.Exclude, cfg.ExcludePatterns)
	if err != nil {
		return nil, fmt.Errorf("watch %q exclusions: %w", cfg.Name, err)
	}
	return &Watcher{
		ing: ing, vaultRoot: vaultRoot, config: cfg, excludes: excludes, mutate: mutate,
		logger: logger, now: time.Now, observations: make(map[string]watchObservation),
	}, nil
}

// Run scans immediately, then at the configured interval until cancellation.
func (w *Watcher) Run(ctx context.Context) error {
	jobs.ReportBusy(ctx, false)
	root, err := w.openRoot(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return w.run(ctx, root, nil)
}

// run serializes startup, periodic reconciliation, and event-hint scans. The
// collector only records bounded hints; it never starts ingestion work.
func (w *Watcher) run(ctx context.Context, root *watchRoot, beforeScan func()) error {
	var source watchNotificationSource
	degraded := false
	blocked := false
	retryEnrollment := w.config.CaptureOnChange
	reportFailure := func(err error) {
		if !degraded {
			jobs.ReportError(ctx, err)
			w.logger.Warn("watch notifications unavailable; polling continues", "watch", w.config.Name, "error", err)
		}
		degraded = true
	}
	disable := func(err error) {
		if source != nil {
			_ = source.Close()
			source = nil
			w.notifications = nil
		}
		retryEnrollment = errors.Is(err, fs.ErrNotExist)
		reportFailure(fmt.Errorf("watch %q notifications: %w", w.config.Name, err))
	}
	enroll := func() {
		if !retryEnrollment {
			return
		}
		factory := w.newNotifications
		if factory == nil {
			factory = func(root string, excludes exclusions) (watchNotificationSource, error) {
				return newWatchNotifications(root, excludes)
			}
		}
		candidate, err := factory(root.path, w.excludes)
		if err != nil {
			disable(err)
			return
		}
		source, w.notifications = candidate, candidate
	}
	scan := func(hints watchScanHints) error {
		if beforeScan != nil {
			beforeScan()
		}
		complete, err := w.scanWithHints(ctx, root, hints)
		if w.notificationErr != nil {
			failure := w.notificationErr
			w.notificationErr = nil
			disable(failure)
		}
		if errors.Is(err, errWatchBlocked) || (err == nil && !complete) {
			if err == nil {
				err = fmt.Errorf("watch %q source inventory incomplete; reconciliation deferred", w.config.Name)
			}
			if !blocked {
				if !degraded {
					jobs.ReportError(ctx, err)
				}
				w.logger.Warn("watch source blocks reconciliation; polling continues", "watch", w.config.Name, "error", err)
			}
			blocked = true
			return nil
		}
		if err != nil {
			return err
		}
		if complete && blocked {
			if !degraded {
				jobs.ReportError(ctx, nil)
			}
			w.logger.Info("watch source reconciliation restored", "watch", w.config.Name)
			blocked = false
		}
		if complete && source != nil && degraded {
			jobs.ReportError(ctx, nil)
			w.logger.Info("watch notifications restored", "watch", w.config.Name)
			degraded = false
		}
		return nil
	}
	enroll()
	if err := scan(watchScanHints{Reconcile: true}); err != nil {
		if source != nil {
			_ = source.Close()
			w.notifications = nil
		}
		return err
	}
	ticker := time.NewTicker(w.config.ScanInterval.Std())
	defer ticker.Stop()
	defer func() {
		if source != nil {
			_ = source.Close()
			w.notifications = nil
		}
	}()
	for {
		var wake <-chan struct{}
		var failures <-chan error
		if source != nil {
			wake, failures = source.Wake(), source.Errors()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if source == nil {
				enroll()
			}
			hints := watchScanHints{Reconcile: true}
			if source != nil {
				hints.Dirty = source.Drain().Dirty
			}
			if err := scan(hints); err != nil {
				return err
			}
		case <-wake:
			hints := source.Drain()
			if err := scan(hints); err != nil {
				return err
			}
		case err := <-failures:
			disable(err)
		}
	}
}

var errWatchCheckoutOverlap = errors.New("watch source overlaps checkout destination")

func (w *Watcher) waitForCheckoutSeparation(ctx context.Context, root string) ([]store.PhotoCheckout, error) {
	var checkouts []store.PhotoCheckout
	err := WaitForCheckoutSeparation(ctx, func(ctx context.Context) error {
		var err error
		checkouts, err = w.ing.Store.ListPhotoCheckouts(ctx)
		if err != nil {
			return backoff.Permanent(err)
		}
		id, overlaps, err := CheckoutPathOverlap(ctx, checkouts, root)
		if err != nil || overlaps {
			destination := ""
			for _, checkout := range checkouts {
				if checkout.ID == id {
					destination = checkout.Destination
					break
				}
			}
			return fmt.Errorf("waiting for checkout %s folder \"%s\"; clear or move its copies from the released folder to start watch %q: %w", id, destination, w.config.Name, errors.Join(err, errWatchCheckoutOverlap))
		}
		return nil
	}, func(err error) {
		jobs.ReportError(ctx, err)
		w.logger.Warn("watch is waiting for checkout separation", "watch", w.config.Name, "path", root, "error", err)
	})
	jobs.ReportError(ctx, nil)
	return checkouts, err
}

func (w *Watcher) openRoot(ctx context.Context) (*watchRoot, error) {
	root, err := filepath.EvalSymlinks(w.config.Source)
	if err != nil {
		return nil, fmt.Errorf("opening watch %q source %q: %w",
			w.config.Name, w.config.Source, err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving watch %q source: %w", w.config.Name, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("checking watch %q source: %w", w.config.Name, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watch %q source %q is not a directory", w.config.Name, root)
	}
	vaultRoot, err := filepath.EvalSymlinks(w.vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving vault root for watch %q: %w", w.config.Name, err)
	}
	overlaps, err := PathsOverlapContext(ctx, root, vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("checking watch %q source against the vault: %w", w.config.Name, err)
	}
	if overlaps {
		return nil, fmt.Errorf("watch %q source %q overlaps the vault root %q",
			w.config.Name, root, vaultRoot)
	}
	checkouts, err := w.waitForCheckoutSeparation(ctx, root)
	if err != nil {
		return nil, err
	}
	if err := ReleaseClearedCheckoutDestinations(ctx, w.ing.Store, checkouts, root); err != nil {
		return nil, err
	}
	vaultInfo, err := os.Stat(vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("checking vault root for watch %q: %w", w.config.Name, err)
	}
	if !vaultInfo.IsDir() {
		return nil, fmt.Errorf("vault root for watch %q is not a directory", w.config.Name)
	}
	vaultDescriptor, err := os.OpenRoot(vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("pinning vault hierarchy for watch %q: %w", w.config.Name, err)
	}
	vaultDirs, identityErr := watchDirectoryIdentities(ctx, vaultDescriptor)
	closeErr := vaultDescriptor.Close()
	if identityErr != nil || closeErr != nil {
		return nil, fmt.Errorf("reading vault hierarchy for watch %q: %w",
			w.config.Name, errors.Join(identityErr, closeErr))
	}
	descriptor, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("pinning watch %q source: %w", w.config.Name, err)
	}
	success := false
	defer func() {
		if !success {
			_ = descriptor.Close()
		}
	}()
	opened, err := descriptor.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("checking pinned watch %q source: %w", w.config.Name, err)
	}
	current, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("rechecking watch %q source: %w", w.config.Name, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(info, opened) || !os.SameFile(current, opened) {
		return nil, fmt.Errorf("watch %q source changed while opening", w.config.Name)
	}
	w.sourceMount, err = watchMountForRoot(descriptor)
	if err != nil {
		return nil, fmt.Errorf("identifying watch %q source mount: %w", w.config.Name, err)
	}
	containsVault, err := watchTreeContainsAnyDirectory(
		ctx, descriptor, w.sourceMount, vaultDirs, w.excludes, "", w.beforeDescend,
	)
	if err != nil {
		return nil, fmt.Errorf("checking watch %q backing hierarchy: %w", w.config.Name, err)
	}
	if containsVault {
		return nil, fmt.Errorf("watch %q source %q contains vault storage through a filesystem alias",
			w.config.Name, root)
	}
	w.vaultDirs = vaultDirs
	success = true
	return &watchRoot{
		path: filepath.Clean(root), root: descriptor, identity: opened,
	}, nil
}

// watchTreeContainsAnyDirectory resolves directory identity through pinned
// handles rather than pathname ancestry. Bind mounts can present any part of
// the vault under an unrelated lexical path, so this check runs before the
// first source file can be ingested. Cross-mount directories are checked for
// an exact vault alias and otherwise remain outside watcher traversal.
func watchTreeContainsAnyDirectory(
	ctx context.Context, dir *os.Root, sourceMount watchMount, targets []fs.FileInfo,
	excludes exclusions, relDir string, beforeDescend func(string),
) (bool, error) {
	info, err := dir.Stat(".")
	if err != nil {
		return false, err
	}
	if matchesWatchDirectory(info, targets) {
		return true, nil
	}
	f, err := dir.Open(".")
	if err != nil {
		return false, err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return false, errors.Join(readErr, closeErr)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		rel := filepath.Join(relDir, entry.Name())
		if excludes.matchRelative(rel) {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if beforeDescend != nil {
			beforeDescend(rel)
		}
		entryInfo, err := dir.Lstat(entry.Name())
		if err != nil {
			if transientWatchTraversalError(err) {
				continue
			}
			return false, err
		}
		if entryInfo.Mode()&fs.ModeSymlink != 0 || !entryInfo.IsDir() {
			continue
		}
		child, err := openWatchDirectory(dir, entry.Name())
		if err != nil {
			if transientWatchTraversalError(err) {
				continue
			}
			return false, err
		}
		childInfo, err := child.Stat(".")
		if err != nil {
			_ = child.Close()
			if transientWatchTraversalError(err) {
				continue
			}
			return false, err
		}
		if matchesWatchDirectory(childInfo, targets) {
			_ = child.Close()
			return true, nil
		}
		childMount, err := watchMountForRoot(child)
		if err != nil {
			_ = child.Close()
			if transientWatchTraversalError(err) {
				continue
			}
			return false, err
		}
		if sourceMount.same(childMount) {
			contains, childErr := watchTreeContainsAnyDirectory(
				ctx, child, sourceMount, targets, excludes, rel, beforeDescend,
			)
			_ = child.Close()
			if childErr != nil {
				if transientWatchTraversalError(childErr) {
					continue
				}
				return false, childErr
			}
			if contains {
				return true, nil
			}
			continue
		}
		_ = child.Close()
	}
	return false, nil
}

func transientWatchTraversalError(err error) bool {
	return errors.Is(err, ErrSourceChanged) || transientWatchObservationError(err)
}

func matchesWatchDirectory(info fs.FileInfo, targets []fs.FileInfo) bool {
	for _, target := range targets {
		if os.SameFile(info, target) {
			return true
		}
	}
	return false
}

func watchDirectoryIdentities(ctx context.Context, dir *os.Root) ([]fs.FileInfo, error) {
	info, err := dir.Stat(".")
	if err != nil {
		return nil, err
	}
	result := []fs.FileInfo{info}
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entryInfo, err := dir.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if entryInfo.Mode()&fs.ModeSymlink != 0 || !entryInfo.IsDir() {
			continue
		}
		child, err := openWatchDirectory(dir, entry.Name())
		if err != nil {
			return nil, err
		}
		children, childErr := watchDirectoryIdentities(ctx, child)
		closeErr := child.Close()
		if childErr != nil || closeErr != nil {
			return nil, errors.Join(childErr, closeErr)
		}
		result = append(result, children...)
	}
	return result, nil
}

// PathsOverlap reports lexical or filesystem-identity ancestry. Storage and
// watched-inbox admission share this check so an aliased vault directory
// cannot become its own input or secondary namespace.
func PathsOverlap(left, right string) (bool, error) {
	return PathsOverlapContext(context.Background(), left, right)
}

func PathsOverlapContext(ctx context.Context, left, right string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if pathContains(left, right) || pathContains(right, left) {
		return true, nil
	}
	resolvedLeft, err := resolveExistingPathPrefixContext(ctx, left)
	if err != nil {
		return false, fmt.Errorf("resolving path %q: %w", left, err)
	}
	resolvedRight, err := resolveExistingPathPrefixContext(ctx, right)
	if err != nil {
		return false, fmt.Errorf("resolving path %q: %w", right, err)
	}
	if pathContains(resolvedLeft, resolvedRight) ||
		pathContains(resolvedRight, resolvedLeft) {
		return true, nil
	}
	var missingErr error
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		overlaps, err := existingAncestorMatchesContext(ctx, pair[0], pair[1])
		if CheckoutPathMissing(err) {
			if missingErr == nil {
				missingErr = err
			}
			continue
		}
		if err != nil {
			return false, err
		}
		if overlaps {
			return true, nil
		}
	}
	return false, missingErr
}

// resolveExistingPathPrefix resolves aliases in the nearest existing prefix
// while retaining any missing suffix for containment comparisons.
func resolveExistingPathPrefixContext(ctx context.Context, pathname string) (string, error) {
	absolute, err := filepath.Abs(pathname)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for _, v := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, v)
			}
			return filepath.Clean(resolved), nil
		}
		if !CheckoutPathMissing(resolveErr) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// WatchBindingOverlap reports whether one filesystem store binding can expose
// its managed objects through a configured watched inbox.
func WatchBindingOverlap(
	watches []config.WatchConfig, binding config.StoreBindingConfig,
) (string, bool, error) {
	return WatchBindingOverlapContext(context.Background(), watches, binding)
}

func WatchBindingOverlapContext(ctx context.Context, watches []config.WatchConfig, binding config.StoreBindingConfig) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if binding.Kind != "filesystem" {
		return "", false, nil
	}
	for _, watch := range watches {
		overlap, err := PathsOverlapContext(ctx, watch.Source, binding.Path)
		if err != nil && !CheckoutPathMissing(err) {
			return watch.Name, false, err
		}
		if !overlap {
			overlap, err = existingPathPrefixMatchesWatch(ctx, binding.Path, watch)
			if err != nil {
				return watch.Name, false, err
			}
		}
		if overlap {
			return watch.Name, true, nil
		}
	}
	return "", false, nil
}

// existingPathPrefixMatchesWatch catches mount aliases that pathname
// resolution cannot expose. It uses the watcher's confined traversal so an
// unrelated mounted tree is not recursively searched. The whole configured
// watch root remains protected even when an exclusion currently hides part of
// it; removing an exclusion must not turn an existing store into watcher input.
func existingPathPrefixMatchesWatch(ctx context.Context, pathname string, watch config.WatchConfig) (bool, error) {
	prefix, ok, err := existingDirectoryPrefix(ctx, pathname)
	if err != nil || !ok {
		return false, err
	}
	return watchTreeContainsAny(ctx, watch, []fs.FileInfo{prefix})
}

// existingDirectoryPrefix returns the identity of pathname's nearest existing
// prefix. It reports false when that prefix is not a directory or no prefix
// exists.
func existingDirectoryPrefix(ctx context.Context, pathname string) (fs.FileInfo, bool, error) {
	current, err := filepath.Abs(pathname)
	if err != nil {
		return nil, false, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		info, err := os.Stat(current)
		if err == nil {
			return info, info.IsDir(), nil
		}
		if !CheckoutPathMissing(err) {
			return nil, false, fmt.Errorf("checking path prefix \"%s\": %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Windows drive and UNC roots can be unavailable while their
			// configured stores remain active but offline. With no observable
			// prefix, there is no filesystem identity to compare to the watch.
			return nil, false, nil
		}
		current = parent
	}
}

// watchTreeContainsAny reports whether any target directory appears in the
// watch source's confined traversal.
func watchTreeContainsAny(ctx context.Context, watch config.WatchConfig, targets []fs.FileInfo) (bool, error) {
	root, err := os.OpenRoot(watch.Source)
	if err != nil {
		return false, fmt.Errorf("opening watch %q source %q: %w", watch.Name, watch.Source, err)
	}
	mount, mountErr := watchMountForRoot(root)
	if mountErr != nil {
		_ = root.Close()
		return false, fmt.Errorf("identifying watch %q source mount: %w", watch.Name, mountErr)
	}
	contains, traversalErr := watchTreeContainsAnyDirectory(
		ctx, root, mount, targets, exclusions{}, "", nil,
	)
	closeErr := root.Close()
	if traversalErr != nil || closeErr != nil {
		return false, fmt.Errorf("reading watch %q source tree: %w",
			watch.Name, errors.Join(traversalErr, closeErr))
	}
	return contains, nil
}

// existingAncestorMatches supplements filepath.Rel with filesystem identity.
// It catches case, normalization, and mount aliases whose lexical paths do
// not reveal that one directory is inside the other.
func existingAncestorMatchesContext(ctx context.Context, pathname, protected string) (bool, error) {
	protectedInfo, err := os.Stat(protected)
	if err != nil {
		return false, fmt.Errorf("checking protected path %q: %w", protected, err)
	}
	current := pathname
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, statErr := os.Stat(current)
		if statErr == nil && os.SameFile(info, protectedInfo) {
			return true, nil
		}
		if statErr != nil && !CheckoutPathMissing(statErr) {
			return false, fmt.Errorf("checking path ancestor %q: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." ||
		(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func (w *Watcher) scan(ctx context.Context, root *watchRoot) error {
	_, err := w.scanWithHints(ctx, root, watchScanHints{Reconcile: true})
	return err
}

func (w *Watcher) scanWithHints(ctx context.Context, root *watchRoot, hints watchScanHints) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if w.notifications != nil {
		w.notifications.BeginScan()
	}
	seen := make(map[string]struct{}, len(w.observations))
	var inventory *watchInventory
	if w.process == nil {
		inventory = &watchInventory{files: make(map[string]watchInventoryEntry), present: make(map[string]struct{}), complete: true,
			known: make(map[string]struct{}), knownDirs: make(map[string]struct{})}
		for rel, observation := range w.observations {
			inventory.known[rel] = struct{}{}
			for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
				inventory.known[dir] = struct{}{}
				inventory.knownDirs[dir] = struct{}{}
			}
			if hints.matches(rel) {
				observation.processed = false
				w.observations[rel] = observation
			}
		}
	}
	if err := w.scanDirectory(ctx, root, root.root, "", seen, hints, inventory); err != nil {
		return false, fmt.Errorf("scanning watch %q: %w", w.config.Name, err)
	}
	retained := make(map[string]struct{})
	if inventory != nil {
		// A complete inventory settles renames before importing. An incomplete
		// one still imports settled files, but holds back new paths that could
		// be renames of accepted sources it did not see.
		var deferred map[string]struct{}
		if inventory.complete {
			var err error
			deferred, retained, err = w.rebindCandidates(ctx, root, inventory)
			if err != nil {
				return false, err
			}
		} else {
			deferred = possibleRenameTargets(w.observations, inventory)
		}
		for _, rel := range inventory.order {
			entry := inventory.files[rel]
			if _, skip := deferred[rel]; skip {
				continue
			}
			if err := w.processObservation(ctx, watchSource{root: root, ref: rel}, entry.state, entry.observedAt, seen, hints); err != nil {
				return false, err
			}
		}
		if !inventory.complete {
			// Unseen paths may only be unreadable: keep their accepted evidence
			// and subscriptions until a complete pass, but time unseen must not
			// count toward settling.
			for rel := range w.observations {
				if _, observed := inventory.files[rel]; !observed {
					w.interruptSettling(rel)
				}
			}
			if len(inventory.blocked) != 0 {
				return false, fmt.Errorf("scanning watch %q: %w", w.config.Name, errors.Join(inventory.blocked...))
			}
			return false, nil
		}
	}
	if w.notifications != nil && w.notificationErr == nil {
		w.notificationErr = w.notifications.EndScan()
	}
	for rel := range w.observations {
		if _, exists := seen[rel]; !exists {
			if _, keep := retained[rel]; !keep {
				delete(w.observations, rel)
			}
		}
	}
	for rel := range w.rejected {
		if _, exists := seen[rel]; !exists {
			delete(w.rejected, rel)
		}
	}
	return true, nil
}

// unreadableDirectoryError marks a directory whose own entries could not be
// listed, as opposed to a failure somewhere below it.
type unreadableDirectoryError struct{ err error }

func (e *unreadableDirectoryError) Error() string { return "listing directory: " + e.err.Error() }
func (e *unreadableDirectoryError) Unwrap() error { return e.err }

// reportsHeldEntry reports whether a transient observation failure must be
// reported rather than skipped. A one-shot push never scans again, so an entry
// another program holds open, such as a Windows sharing or lock violation,
// would otherwise be left out silently. A watch scan retries it later, and an
// entry that disappeared needs no report.
func (w *Watcher) reportsHeldEntry(err error) bool {
	return w.immediate && w.reject != nil && !errors.Is(err, fs.ErrNotExist)
}

// rejectEntry reports an unreadable file or directory once, until it becomes
// readable or disappears, and lets the scan continue with its siblings.
func (w *Watcher) rejectEntry(ctx context.Context, rel string, err error, seen map[string]struct{}) error {
	seen[rel] = struct{}{}
	delete(w.observations, rel)
	if _, reported := w.rejected[rel]; reported {
		return nil
	}
	w.rejected[rel] = struct{}{}
	return w.reject(ctx, rel, err)
}

var errWatchBlocked = errors.New("watched source blocks reconciliation")

// blockWatchPath skips a known path whose structure changed and defers
// reconciliation until it is restored. Time spent hidden behind the change
// must not count toward settling anything below rel.
func (w *Watcher) blockWatchPath(inventory *watchInventory, rel, reason string) {
	w.interruptSettlingTree(rel)
	inventory.complete = false
	inventory.blocked = append(inventory.blocked, fmt.Errorf("watched source %q %s: %w", rel, reason, errWatchBlocked))
}

func retryableWatchCollectionError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A confirmed replacement remains retryable when Windows also reports
	// access denied while opening the new directory. Permission failures without
	// this source-change evidence remain permanent collection failures.
	if errors.Is(err, ErrSourceChanged) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if cause != nil && !retryableWatchCollectionError(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return retryableWatchCollectionError(cause)
	}
	return errors.Is(err, ErrSourceChanged) || transientWatchObservationError(err)
}

func (w *Watcher) scanDirectory(
	ctx context.Context, root *watchRoot, dir *os.Root, relDir string,
	seen map[string]struct{}, hints watchScanHints, inventory *watchInventory,
) error {
	if err := root.verify(); err != nil {
		return err
	}
	f, err := dir.Open(".")
	if err != nil {
		failure := &unreadableDirectoryError{err: err}
		if inventory != nil && retryableWatchCollectionError(failure) {
			w.interruptSettlingTree(relDir)
		}
		return failure
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		failure := &unreadableDirectoryError{err: errors.Join(readErr, closeErr)}
		if inventory != nil && retryableWatchCollectionError(failure) {
			w.interruptSettlingTree(relDir)
		}
		return failure
	}
	if w.notifications != nil && w.notificationErr == nil {
		w.notificationErr = w.notifications.AddDirectory(relDir, len(entries))
	}
	slices.SortFunc(entries, func(left, right os.DirEntry) int {
		return strings.Compare(left.Name(), right.Name())
	})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(relDir, entry.Name())
		if inventory != nil {
			inventory.present[rel] = struct{}{}
		}
		filePath := filepath.Join(root.path, filepath.FromSlash(rel))
		if w.excludes.match(root.path, filePath) {
			if inventory != nil {
				if _, known := inventory.known[rel]; known {
					w.blockWatchPath(inventory, rel, "became excluded")
					continue
				}
			}
			if w.excluded != nil {
				if err := w.excluded(ctx, rel); err != nil {
					return err
				}
			}
			continue
		}
		info, err := dir.Lstat(entry.Name())
		if err != nil {
			if inventory != nil {
				if retryableWatchCollectionError(err) {
					w.interruptSettlingTree(rel)
					inventory.complete = false
					continue
				}
				return fmt.Errorf("checking entry %q: %w", rel, err)
			}
			if transientWatchObservationError(err) && !w.reportsHeldEntry(err) {
				continue
			}
			if w.reject != nil {
				if err := w.rejectEntry(ctx, rel, err, seen); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("checking entry %q: %w", rel, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if inventory != nil {
				if _, known := inventory.knownDirs[rel]; known {
					w.blockWatchPath(inventory, rel, "became a symlink")
					continue
				}
			}
			if w.excluded != nil {
				if err := w.excluded(ctx, rel); err != nil {
					return err
				}
			}
			continue
		}
		if info.IsDir() {
			if w.beforeDescend != nil {
				w.beforeDescend(rel)
			}
			child, err := openWatchDirectory(dir, entry.Name())
			if err != nil {
				if inventory != nil {
					if retryableWatchCollectionError(err) {
						w.interruptSettlingTree(rel)
						inventory.complete = false
						continue
					}
					return fmt.Errorf("opening directory %q: %w", rel, err)
				}
				if errors.Is(err, ErrSourceChanged) ||
					(transientWatchObservationError(err) && !w.reportsHeldEntry(err)) {
					continue
				}
				if w.reject != nil {
					if err := w.rejectEntry(ctx, rel, err, seen); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("opening directory %q: %w", rel, err)
			}
			childInfo, infoErr := child.Stat(".")
			if infoErr != nil {
				_ = child.Close()
				if inventory != nil && retryableWatchCollectionError(infoErr) {
					w.interruptSettlingTree(rel)
					inventory.complete = false
					continue
				}
				return fmt.Errorf("checking directory %q identity: %w", rel, infoErr)
			}
			if matchesWatchDirectory(childInfo, w.vaultDirs) {
				_ = child.Close()
				return fmt.Errorf("directory %q is vault storage through a filesystem alias", rel)
			}
			childMount, mountErr := watchMountForRoot(child)
			if mountErr != nil {
				_ = child.Close()
				if inventory != nil && retryableWatchCollectionError(mountErr) {
					w.interruptSettlingTree(rel)
					inventory.complete = false
					continue
				}
				return fmt.Errorf("checking directory %q filesystem boundary: %w", rel, mountErr)
			}
			if !w.sourceMount.same(childMount) {
				if inventory != nil {
					if _, known := inventory.known[rel]; known {
						_ = child.Close()
						w.blockWatchPath(inventory, rel, "crossed a filesystem boundary")
						continue
					}
				}
				_ = child.Close()
				if w.excluded != nil {
					if err := w.excluded(ctx, rel); err != nil {
						return err
					}
				}
				continue
			}
			scanErr := w.scanDirectory(ctx, root, child, rel, seen, hints, inventory)
			_ = child.Close()
			if scanErr != nil {
				if inventory != nil {
					if retryableWatchCollectionError(scanErr) {
						w.interruptSettlingTree(rel)
						inventory.complete = false
						continue
					}
					return fmt.Errorf("scanning directory %q: %w", rel, scanErr)
				}
				if errors.Is(scanErr, ErrSourceChanged) && !w.immediate {
					continue
				}
				if unreadable, ok := errors.AsType[*unreadableDirectoryError](scanErr); ok && w.reject != nil {
					if err := w.rejectEntry(ctx, rel, unreadable.err, seen); err != nil {
						return err
					}
					continue
				}
				return scanErr
			}
			if inventory == nil {
				delete(w.rejected, rel)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			if inventory != nil {
				if _, known := inventory.known[rel]; known {
					w.blockWatchPath(inventory, rel, "became nonregular")
					continue
				}
			}
			if w.excluded != nil {
				if err := w.excluded(ctx, rel); err != nil {
					return err
				}
			}
			continue
		}
		if inventory != nil {
			if _, wasDir := inventory.knownDirs[rel]; wasDir {
				w.blockWatchPath(inventory, rel, "became a file")
				continue
			}
		}
		if err := validateSourcePath(rel); err != nil {
			if w.reject != nil {
				if err := w.rejectEntry(ctx, rel, err, seen); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if w.beforeFile != nil {
			if err := w.beforeFile(ctx, rel); err != nil {
				if errors.Is(err, filepath.SkipDir) {
					continue
				}
				return err
			}
		}
		source := watchSource{root: root, ref: rel}
		if w.beforeObserve != nil {
			w.beforeObserve(rel)
		}
		observe := source.observe
		if w.observeFile != nil {
			observe = func(withChange bool) (watchFileState, error) { return w.observeFile(source, withChange) }
		}
		state, err := observe(w.process == nil)
		if err != nil {
			if inventory != nil {
				if retryableWatchCollectionError(err) {
					w.interruptSettling(rel)
					inventory.complete = false
					continue
				}
				return fmt.Errorf("observing entry %q: %w", rel, err)
			}
			if errors.Is(err, ErrSourceChanged) {
				if w.immediate && w.sourceChanged != nil {
					if changeErr := w.sourceChanged(ctx, rel, err); changeErr != nil {
						return changeErr
					}
					seen[rel] = struct{}{}
					continue
				}
				w.interruptSettling(rel)
				if w.immediate {
					return err
				}
				continue
			}
			if transientWatchObservationError(err) && !w.reportsHeldEntry(err) {
				w.interruptSettling(rel)
				continue
			}
			if w.reject != nil {
				if err := w.rejectEntry(ctx, rel, err, seen); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("checking entry %q: %w", rel, err)
		}
		if !w.sourceMount.same(state.mount) {
			if inventory != nil {
				if _, known := inventory.known[rel]; known {
					w.blockWatchPath(inventory, rel, "crossed a filesystem boundary")
					continue
				}
			}
			if w.excluded != nil {
				if err := w.excluded(ctx, rel); err != nil {
					return err
				}
			}
			continue
		}
		observedAt := w.now()
		if inventory != nil {
			inventory.files[rel] = watchInventoryEntry{state: state, observedAt: observedAt}
			inventory.order = append(inventory.order, rel)
			continue
		}
		if err := w.processObservation(ctx, source, state, observedAt, seen, hints); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) processObservation(ctx context.Context, source watchSource, state watchFileState, observedAt time.Time, seen map[string]struct{}, hints watchScanHints) error {
	rel := source.ref
	fingerprint := state.fingerprint
	var err error
	seen[rel] = struct{}{}
	observation, exists := w.observations[rel]
	fingerprintChanged := !exists || !observation.fingerprint.matches(fingerprint)
	if fingerprintChanged || observation.settlingInterrupted {
		observation = watchObservation{
			fingerprint: fingerprint, stableSince: observedAt, digest: observation.digest, accepted: observation.accepted,
		}
		w.observations[rel] = observation
	}
	if hints.matches(rel) && exists {
		observation.processed = false
	}
	if observation.digest != "" && (hints.Reconcile || hints.matches(rel) || fingerprintChanged) {
		var digest string
		digest, err = source.digest(ctx, fingerprint)
		if err == nil && digest != observation.digest {
			observation = watchObservation{
				fingerprint: fingerprint, stableSince: observedAt, digest: digest, accepted: observation.accepted,
			}
			w.observations[rel] = observation
		}
	}
	var result WatchResult
	if err == nil {
		minimumAge := w.config.MinimumAge.Std()
		if observation.processed ||
			(!w.immediate && !w.config.CaptureOnChange && observedAt.Sub(observation.stableSince) < w.config.SettleTime.Std()) ||
			(minimumAge > 0 && observedAt.Sub(time.Unix(0, fingerprint.modTime)) < minimumAge) {
			return nil
		}
		jobs.ReportBusy(ctx, true)
		if w.process != nil {
			err = w.process(ctx, rel, &observation, source.open)
		} else {
			err = w.mutate(func() error {
				return w.ing.Blobs.WithMutation(ctx, func() error {
					var writeErr error
					result, writeErr = w.ing.ingestWatchedFile(
						ctx, w.config.Name, w.config.Destination, rel, fingerprint, source.open,
					)
					return writeErr
				})
			})
		}
		jobs.ReportBusy(ctx, false)
	}
	if access, ok := errors.AsType[*SourceAccessError](err); ok && w.reject != nil &&
		!errors.Is(err, ErrSourceChanged) &&
		(!transientWatchObservationError(err) || w.reportsHeldEntry(err)) {
		if err := w.rejectEntry(ctx, rel, access.Err, seen); err != nil {
			return err
		}
		return nil
	}
	if errors.Is(err, ErrSourceChanged) && w.immediate && w.sourceChanged != nil {
		if changeErr := w.sourceChanged(ctx, rel, err); changeErr != nil {
			return changeErr
		}
		return nil
	}
	if errors.Is(err, ErrSourceChanged) || errors.Is(err, ErrRetryLater) ||
		transientWatchObservationError(err) {
		if w.immediate {
			return err
		}
		if observation.digest == "" && observation.accepted == nil {
			delete(w.observations, rel)
		} else {
			observation.processed = false
			observation.stableSince = w.now()
			w.observations[rel] = observation
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingesting %q: %w", rel, err)
	}
	delete(w.rejected, rel)
	observation.processed = true
	if w.process == nil {
		observation.accepted = &watchAcceptedSource{nodeID: result.Node.ID, identity: state.identity, info: state.info}
	}
	w.observations[rel] = observation
	if w.process == nil {
		w.logger.Info("watched inbox file processed", "watch", w.config.Name,
			"source", rel, "node_id", result.Node.ID, "outcome", result.Outcome)
	}
	return nil
}

func (w *Watcher) interruptSettling(rel string) {
	observation, exists := w.observations[rel]
	if !exists {
		return
	}
	if observation.digest == "" && observation.accepted == nil {
		delete(w.observations, rel)
		return
	}
	observation.processed = false
	observation.settlingInterrupted = true
	w.observations[rel] = observation
}

func (w *Watcher) interruptSettlingTree(rel string) {
	prefix := rel + "/"
	for ref := range w.observations {
		if rel == "" || ref == rel || strings.HasPrefix(ref, prefix) {
			w.interruptSettling(ref)
		}
	}
}
