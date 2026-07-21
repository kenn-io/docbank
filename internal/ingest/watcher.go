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

	"go.kenn.io/docbank/internal/config"
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
) (WatchResult, error) {
	var result WatchResult
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
	fingerprint watchFingerprint
	stableSince time.Time
	processed   bool
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
	f, err := dir.Open(name)
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

func (source watchSource) observe() (watchFingerprint, watchMount, error) {
	var noMount watchMount
	f, err := source.open()
	if err != nil {
		return watchFingerprint{}, noMount, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return watchFingerprint{}, noMount, err
	}
	mount, err := watchMountForFile(f)
	if err != nil {
		return watchFingerprint{}, noMount, err
	}
	return fingerprintFileInfo(info), mount, nil
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

// Watcher polls one configured local inbox. Its observations are deliberately
// in memory: after every daemon restart a file must prove stability for a full
// settle window again before Docbank opens it.
type Watcher struct {
	ing           *Ingester
	vaultRoot     string
	vaultDirs     []fs.FileInfo
	config        config.WatchConfig
	excludes      exclusions
	mutate        func(func() error) error
	logger        *slog.Logger
	now           func() time.Time
	beforeDescend func(string)
	sourceMount   watchMount
	observations  map[string]watchObservation
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
	excludes, err := compileExclusions(cfg.Exclude)
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
	root, err := w.openRoot(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := w.scan(ctx, root); err != nil {
		return err
	}
	ticker := time.NewTicker(w.config.ScanInterval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.scan(ctx, root); err != nil {
				return err
			}
		}
	}
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
	overlaps, err := pathsOverlap(root, vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("checking watch %q source against the vault: %w", w.config.Name, err)
	}
	if overlaps {
		return nil, fmt.Errorf("watch %q source %q overlaps the vault root %q",
			w.config.Name, root, vaultRoot)
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

func pathsOverlap(left, right string) (bool, error) {
	if pathContains(left, right) || pathContains(right, left) {
		return true, nil
	}
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		overlaps, err := existingAncestorMatches(pair[0], pair[1])
		if err != nil {
			return false, err
		}
		if overlaps {
			return true, nil
		}
	}
	return false, nil
}

// existingAncestorMatches supplements filepath.Rel with filesystem identity.
// It catches case, normalization, and mount aliases whose lexical paths do
// not reveal that one directory is inside the other.
func existingAncestorMatches(pathname, protected string) (bool, error) {
	protectedInfo, err := os.Stat(protected)
	if err != nil {
		return false, fmt.Errorf("checking protected path %q: %w", protected, err)
	}
	current := pathname
	for {
		info, statErr := os.Stat(current)
		if statErr == nil && os.SameFile(info, protectedInfo) {
			return true, nil
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
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
	seen := make(map[string]struct{}, len(w.observations))
	if err := w.scanDirectory(ctx, root, root.root, "", seen); err != nil {
		return fmt.Errorf("scanning watch %q: %w", w.config.Name, err)
	}
	for rel := range w.observations {
		if _, exists := seen[rel]; !exists {
			delete(w.observations, rel)
		}
	}
	return nil
}

func (w *Watcher) scanDirectory(
	ctx context.Context, root *watchRoot, dir *os.Root, relDir string,
	seen map[string]struct{},
) error {
	if err := root.verify(); err != nil {
		return err
	}
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	slices.SortFunc(entries, func(left, right os.DirEntry) int {
		return strings.Compare(left.Name(), right.Name())
	})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(relDir, entry.Name())
		filePath := filepath.Join(root.path, filepath.FromSlash(rel))
		if w.excludes.match(root.path, filePath) {
			continue
		}
		info, err := dir.Lstat(entry.Name())
		if err != nil {
			if transientWatchObservationError(err) {
				continue
			}
			return fmt.Errorf("checking entry %q: %w", rel, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if info.IsDir() {
			if w.beforeDescend != nil {
				w.beforeDescend(rel)
			}
			child, err := openWatchDirectory(dir, entry.Name())
			if err != nil {
				if errors.Is(err, ErrSourceChanged) || transientWatchObservationError(err) {
					continue
				}
				return fmt.Errorf("opening directory %q: %w", rel, err)
			}
			childInfo, infoErr := child.Stat(".")
			if infoErr != nil {
				_ = child.Close()
				return fmt.Errorf("checking directory %q identity: %w", rel, infoErr)
			}
			if matchesWatchDirectory(childInfo, w.vaultDirs) {
				_ = child.Close()
				return fmt.Errorf("directory %q is vault storage through a filesystem alias", rel)
			}
			childMount, mountErr := watchMountForRoot(child)
			if mountErr != nil {
				_ = child.Close()
				return fmt.Errorf("checking directory %q filesystem boundary: %w", rel, mountErr)
			}
			if !w.sourceMount.same(childMount) {
				_ = child.Close()
				continue
			}
			scanErr := w.scanDirectory(ctx, root, child, rel, seen)
			_ = child.Close()
			if scanErr != nil {
				if errors.Is(scanErr, ErrSourceChanged) {
					continue
				}
				return scanErr
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := validateSourcePath(rel); err != nil {
			return err
		}
		source := watchSource{root: root, ref: rel}
		fingerprint, sourceMount, err := source.observe()
		if err != nil {
			if errors.Is(err, ErrSourceChanged) || transientWatchObservationError(err) {
				delete(w.observations, rel)
				continue
			}
			return fmt.Errorf("checking entry %q: %w", rel, err)
		}
		if !w.sourceMount.same(sourceMount) {
			continue
		}
		observedAt := w.now()
		seen[rel] = struct{}{}
		observation, exists := w.observations[rel]
		if !exists || !observation.fingerprint.matches(fingerprint) {
			w.observations[rel] = watchObservation{
				fingerprint: fingerprint, stableSince: observedAt,
			}
			continue
		}
		if observation.processed || observedAt.Sub(observation.stableSince) < w.config.SettleTime.Std() {
			continue
		}
		var result WatchResult
		err = w.mutate(func() error {
			return w.ing.Blobs.WithMutation(ctx, func() error {
				var writeErr error
				result, writeErr = w.ing.ingestWatchedFile(
					ctx, w.config.Name, w.config.Destination, rel, fingerprint, source.open,
				)
				return writeErr
			})
		})
		if errors.Is(err, ErrSourceChanged) || transientWatchObservationError(err) {
			delete(w.observations, rel)
			continue
		}
		if err != nil {
			return fmt.Errorf("ingesting %q: %w", rel, err)
		}
		observation.processed = true
		w.observations[rel] = observation
		w.logger.Info("watched inbox file processed", "watch", w.config.Name,
			"source", rel, "node_id", result.Node.ID, "outcome", result.Outcome)
	}
	return nil
}
