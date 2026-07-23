package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/kit/safefileio"
)

// LockPath is the advisory lock file serializing vault access across
// processes: the daemon holds it exclusively for its entire lifetime.
func (l Layout) LockPath() string { return filepath.Join(l.Root, "vault.lock") }

// Lock is a held advisory lock on the vault.
type Lock struct {
	files []*os.File
}

// OwnershipTransition excludes ordinary owner acquisition through one stable
// parent directory while a child vault root changes identity. Reset uses it
// across the release, rename, and fresh-vault acquisition boundary.
type OwnershipTransition struct {
	state *ownershipTransitionState
}

type ownershipTransitionState struct {
	layout         Layout
	lock           *Lock
	parentIdentity string
	replacement    *Lock
	parentIndex    int
}

var errLockWouldBlock = errors.New("file lock would block")

// ErrVaultLocked is returned by TryLockExclusive when another process
// already holds the vault lock.
var ErrVaultLocked = errors.New("vault is locked by another process")

// TryLockExclusive takes the vault lock without blocking. The daemon is the
// single lock holder for the vault's lifetime; a second daemon (or a stale
// holder) surfaces immediately instead of hanging.
func (l Layout) TryLockExclusive() (*Lock, error) {
	root, lock, err := l.OpenExistingAndLockExclusive()
	if root != nil {
		_ = root.Close()
	}
	return lock, err
}

// OpenExistingAndLockExclusive opens and exclusively owns an existing vault
// root without creating any missing path component.
func (l Layout) OpenExistingAndLockExclusive() (*os.Root, *Lock, error) {
	acquisition, err := l.tryLockOwnershipAcquisition()
	if err != nil {
		return nil, nil, err
	}
	root, lock, openErr := l.openExistingAndLockExclusive()
	releaseErr := acquisition.Release()
	if releaseErr != nil {
		if lock != nil {
			releaseErr = errors.Join(releaseErr, lock.Release())
		}
		if root != nil {
			releaseErr = errors.Join(releaseErr, root.Close())
		}
		return nil, nil, errors.Join(openErr, releaseErr)
	}
	return root, lock, openErr
}

func (l Layout) openExistingAndLockExclusive() (*os.Root, *Lock, error) {
	root, err := os.OpenRoot(l.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("opening vault root %s: %w", l.Root, err)
	}
	lock, err := l.tryLockExclusiveRoot(root)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return root, lock, nil
}

// TryLockOwnershipTransition exclusively owns the vault root's stable parent
// identity. Ordinary owners briefly share its ancestors and exclusively claim
// their deepest existing identity while taking durable hierarchy locks.
func (l Layout) TryLockOwnershipTransition() (*OwnershipTransition, error) {
	target, err := CanonicalRoot(l.Root)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(target)
	before, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("checking vault transition parent identity: %w", err)
	}
	identities, err := directoryIdentityChain(parent)
	if err != nil {
		return nil, err
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return nil, err
	}
	lock := &Lock{}
	if err := lock.lockAcquisitionIdentities(
		registry, identities, true, target); err != nil {
		_ = lock.Release()
		return nil, err
	}
	after, err := os.Stat(parent)
	if err != nil || !os.SameFile(before, after) {
		_ = lock.Release()
		if err != nil {
			return nil, fmt.Errorf("rechecking vault transition parent identity: %w", err)
		}
		return nil, fmt.Errorf("vault transition parent %s changed while locking", parent)
	}
	return &OwnershipTransition{state: &ownershipTransitionState{
		layout:         Layout{Root: target},
		lock:           lock,
		parentIdentity: identities[len(identities)-1],
	}}, nil
}

// OpenExistingAndLockExclusive validates and owns an existing root while this
// transition keeps competing ownership acquisition excluded.
func (t *OwnershipTransition) OpenExistingAndLockExclusive() (*os.Root, *Lock, error) {
	if t == nil || t.state == nil || t.state.lock == nil {
		return nil, nil, errors.New("vault ownership transition is not held")
	}
	return t.state.layout.openExistingAndLockExclusive()
}

// OpenAndLockExclusive opens or creates a root while this transition keeps
// competing ownership acquisition excluded.
func (t *OwnershipTransition) OpenAndLockExclusive() (*os.Root, *Lock, error) {
	if t == nil || t.state == nil || t.state.lock == nil {
		return nil, nil, errors.New("vault ownership transition is not held")
	}
	return t.state.layout.openAndLockExclusive()
}

// OpenExistingForReplacement owns the existing source and promotes its parent
// identity lock to an exclusive legacy-compatible barrier. The source target
// lock remains held while the parent lock changes mode, so a competing legacy
// owner can only make the promotion fail, not enter the source hierarchy. A
// legacy sibling owner necessarily prevents promotion because its ordinary
// lifetime lock shares that parent identity; reset then fails before mutation.
func (t *OwnershipTransition) OpenExistingForReplacement() (*os.Root, error) {
	if t == nil || t.state == nil || t.state.lock == nil {
		return nil, errors.New("vault ownership transition is not held")
	}
	if t.state.replacement != nil {
		return nil, errors.New("vault ownership replacement is already active")
	}
	root, replacement, err := t.state.layout.openExistingAndLockExclusive()
	if err != nil {
		return nil, err
	}
	parentIndex := len(replacement.files) - 3
	if parentIndex < 0 {
		err = errors.New("vault ownership replacement has no parent hierarchy lock")
		return nil, errors.Join(err, replacement.Release(), root.Close())
	}
	if err := replacement.relockAt(parentIndex, true, t.state.layout.Root); err != nil {
		return nil, errors.Join(err, replacement.Release(), root.Close())
	}
	t.state.replacement = replacement
	t.state.parentIndex = parentIndex
	return root, nil
}

// ReleaseSourceForReplacement drops the old target identity and local vault
// locks while retaining the promoted parent hierarchy barrier for rename.
func (t *OwnershipTransition) ReleaseSourceForReplacement() error {
	if t == nil || t.state == nil || t.state.replacement == nil {
		return errors.New("vault ownership replacement is not active")
	}
	return t.state.replacement.releaseFrom(t.state.parentIndex + 1)
}

// OpenAndLockReplacement creates or opens the replacement beneath the held
// parent barrier, takes its target and local locks, then downgrades the parent
// to the shared mode retained by an ordinary vault owner.
func (t *OwnershipTransition) OpenAndLockReplacement() (*os.Root, *Lock, error) {
	if t == nil || t.state == nil || t.state.lock == nil ||
		t.state.replacement == nil {
		return nil, nil, errors.New("vault ownership replacement is not active")
	}
	replacement := t.state.replacement
	if len(replacement.files) != t.state.parentIndex+1 {
		return nil, nil, errors.New("vault ownership replacement source is still held")
	}
	target := t.state.layout.Root
	parent := filepath.Dir(target)
	component := filepath.Base(target)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return nil, nil, t.failReplacement(
			nil, fmt.Errorf("checking replacement parent identity: %w", err))
	}
	parentIdentity, err := directoryIdentity(parentInfo, parent)
	if err != nil {
		return nil, nil, t.failReplacement(nil, err)
	}
	if parentIdentity != t.state.parentIdentity {
		return nil, nil, t.failReplacement(
			nil, fmt.Errorf("vault replacement parent %s changed while held", parent))
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, t.failReplacement(
			nil, fmt.Errorf("opening replacement parent: %w", err))
	}
	heldParent, err := parentRoot.Stat(".")
	if err != nil || !os.SameFile(parentInfo, heldParent) {
		_ = parentRoot.Close()
		if err != nil {
			return nil, nil, t.failReplacement(
				nil, fmt.Errorf("checking held replacement parent: %w", err))
		}
		return nil, nil, t.failReplacement(
			nil, fmt.Errorf("vault replacement parent %s changed while opening", parent))
	}
	root, enterErr := enterVaultDir(parentRoot, component)
	closeParentErr := parentRoot.Close()
	if err := errors.Join(enterErr, closeParentErr); err != nil {
		if root != nil {
			_ = root.Close()
		}
		return nil, nil, t.failReplacement(
			nil, fmt.Errorf("creating replacement vault root: %w", err))
	}
	fail := func(cause error) (*os.Root, *Lock, error) {
		return nil, nil, t.failReplacement(root, cause)
	}
	info, err := root.Stat(".")
	if err != nil {
		return fail(fmt.Errorf("checking replacement vault root: %w", err))
	}
	identity, err := directoryIdentity(info, target)
	if err != nil {
		return fail(err)
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return fail(err)
	}
	if err := replacement.lockIdentities(
		registry, []string{identity}, true, target); err != nil {
		return fail(err)
	}
	local, err := openRootLock(root)
	if err != nil {
		return fail(err)
	}
	if err := lockFile(local, true); err != nil {
		_ = local.Close()
		return fail(classifyLockError(err, target))
	}
	replacement.files = append(replacement.files, local)
	if err := verifyLayoutRoot(target, root); err != nil {
		return fail(err)
	}
	if err := replacement.relockAt(t.state.parentIndex, false, target); err != nil {
		return fail(err)
	}
	t.state.replacement = nil
	return root, replacement, nil
}

func (t *OwnershipTransition) failReplacement(root *os.Root, cause error) error {
	if root != nil {
		cause = errors.Join(cause, root.Close())
	}
	if t != nil && t.state != nil && t.state.replacement != nil {
		cause = errors.Join(cause, t.state.replacement.Release())
		t.state.replacement = nil
	}
	return cause
}

// Release drops the hierarchy-wide acquisition and replacement barriers.
func (t *OwnershipTransition) Release() error {
	if t == nil || t.state == nil {
		return nil
	}
	var replacementErr, acquisitionErr error
	if t.state.replacement != nil {
		replacementErr = t.state.replacement.Release()
		t.state.replacement = nil
	}
	if t.state.lock != nil {
		acquisitionErr = t.state.lock.Release()
		t.state.lock = nil
	}
	return errors.Join(replacementErr, acquisitionErr)
}

// OpenAndLockExclusive opens an existing vault root or creates a missing one
// through a held parent while coordinating every existing ancestor. The
// returned root is the same directory protected by the returned lock.
func (l Layout) OpenAndLockExclusive() (*os.Root, *Lock, error) {
	acquisition, err := l.tryLockOwnershipAcquisition()
	if err != nil {
		return nil, nil, err
	}
	root, lock, openErr := l.openAndLockExclusive()
	releaseErr := acquisition.Release()
	if releaseErr != nil {
		if lock != nil {
			releaseErr = errors.Join(releaseErr, lock.Release())
		}
		if root != nil {
			releaseErr = errors.Join(releaseErr, root.Close())
		}
		return nil, nil, errors.Join(openErr, releaseErr)
	}
	return root, lock, openErr
}

func (l Layout) openAndLockExclusive() (*os.Root, *Lock, error) {
	root, err := os.OpenRoot(l.Root)
	if err == nil {
		lk, lockErr := l.tryLockExclusiveRoot(root)
		if lockErr != nil {
			_ = root.Close()
			return nil, nil, lockErr
		}
		return root, lk, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("opening vault root %s: %w", l.Root, err)
	}
	return l.createAndLockExclusive()
}

// TryLockLaunch serializes daemon starters without creating or modifying any
// vault root. Startup uses one short-lived per-user lock because missing paths
// cannot be compared reliably across filesystem-specific case, Unicode, and
// mount-point rules. A started daemon's ordinary vault locks remain per-tree.
func (l Layout) TryLockLaunch() (*Lock, error) {
	target, err := filepath.Abs(l.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving vault root %s: %w", l.Root, err)
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return nil, err
	}
	f, err := openRegistryLock(registry, "daemon-launch.lock")
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, true); err != nil {
		_ = f.Close()
		return nil, classifyLockError(err, target)
	}
	return &Lock{files: []*os.File{f}}, nil
}

// TryLockExistingAncestors takes shared hierarchy locks through the deepest
// existing ancestor of a possibly missing vault root without creating anything
// in that tree. A caller can retain this while another component securely
// creates the final path, then acquire exclusive target ownership before
// releasing it.
func (l Layout) TryLockExistingAncestors() (*Lock, error) {
	target, err := CanonicalRoot(l.Root)
	if err != nil {
		return nil, err
	}
	current := target
	for {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("checking vault root ancestor: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("vault root has no existing ancestor: %s", target)
		}
		current = parent
	}
	before, err := os.Stat(current)
	if err != nil {
		return nil, fmt.Errorf("checking vault root ancestor identity: %w", err)
	}
	identities, err := directoryIdentityChain(current)
	if err != nil {
		return nil, err
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return nil, err
	}
	lk := &Lock{}
	if err := lk.lockIdentities(registry, identities, false, target); err != nil {
		_ = lk.Release()
		return nil, err
	}
	after, err := os.Stat(current)
	if err != nil || !os.SameFile(before, after) {
		_ = lk.Release()
		if err != nil {
			return nil, fmt.Errorf("rechecking vault root ancestor identity: %w", err)
		}
		return nil, fmt.Errorf("vault root ancestor %s changed while locking", current)
	}
	return lk, nil
}

// OpenLaunchOutput creates private transient bootstrap output outside the
// unowned vault tree. The caller closes and removes the returned path.
func (l Layout) OpenLaunchOutput() (*os.File, string, error) {
	registry, err := targetLockRegistryDir()
	if err != nil {
		return nil, "", err
	}
	f, err := os.CreateTemp(registry, "daemon-start-*.log")
	if err != nil {
		return nil, "", fmt.Errorf("creating daemon bootstrap output: %w", err)
	}
	return f, f.Name(), nil
}

// TryLockExclusiveRoot coordinates an exclusive vault owner against the exact
// borrowed directory descriptor it will mutate. Shared locks on every ancestor
// identity make an owner conflict with restores of parent or descendant trees;
// the target's ordinary vault.lock preserves coordination with older builds.
func (l Layout) TryLockExclusiveRoot(root *os.Root) (*Lock, error) {
	acquisition, err := l.tryLockOwnershipAcquisition()
	if err != nil {
		return nil, err
	}
	lock, lockErr := l.tryLockExclusiveRoot(root)
	releaseErr := acquisition.Release()
	if releaseErr != nil {
		if lock != nil {
			releaseErr = errors.Join(releaseErr, lock.Release())
		}
		return nil, errors.Join(lockErr, releaseErr)
	}
	return lock, lockErr
}

func (l Layout) tryLockExclusiveRoot(root *os.Root) (*Lock, error) {
	target, err := filepath.Abs(l.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving vault root %s: %w", l.Root, err)
	}
	if err := verifyLayoutRoot(target, root); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, fmt.Errorf("resolving vault root %s: %w", target, err)
	}
	identities, err := directoryIdentityChain(resolved)
	if err != nil {
		return nil, err
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return nil, err
	}

	lk := &Lock{}
	failed := true
	defer func() {
		if failed {
			_ = lk.Release()
		}
	}()
	if err := lk.lockIdentities(registry, identities, true, target); err != nil {
		return nil, err
	}

	local, err := openRootLock(root)
	if err != nil {
		return nil, err
	}
	if err := lockFile(local, true); err != nil {
		_ = local.Close()
		return nil, classifyLockError(err, target)
	}
	lk.files = append(lk.files, local)
	if err := verifyLayoutRoot(target, root); err != nil {
		return nil, err
	}
	failed = false
	return lk, nil
}

func (l Layout) createAndLockExclusive() (*os.Root, *Lock, error) {
	return l.createAndLockExclusiveWith(nil, nil)
}

func (l Layout) createAndLockExclusiveWith(
	beforeScan func() error,
	afterOpen func(int, *os.Root) error,
) (*os.Root, *Lock, error) {
	if beforeScan != nil {
		if err := beforeScan(); err != nil {
			return nil, nil, err
		}
	}
	target, err := filepath.Abs(l.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving vault root %s: %w", l.Root, err)
	}
	current := target
	var missing []string
	for {
		_, err = os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("checking vault root ancestor: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, nil, fmt.Errorf("vault root has no existing ancestor: %s", target)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	ancestor, err := os.Stat(current)
	if err != nil {
		return nil, nil, fmt.Errorf("checking vault root ancestor identity: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving vault root ancestor: %w", err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("opening vault root ancestor: %w", err)
	}
	fail := func(lk *Lock, cause error) (*os.Root, *Lock, error) {
		_ = root.Close()
		if lk != nil {
			_ = lk.Release()
		}
		return nil, nil, cause
	}
	heldAncestor, err := root.Stat(".")
	if err != nil {
		return fail(nil, fmt.Errorf("checking held vault root ancestor: %w", err))
	}
	if !os.SameFile(ancestor, heldAncestor) {
		return fail(nil, fmt.Errorf(
			"vault root ancestor %s was replaced while opening it", current))
	}
	ancestorIdentities, err := directoryIdentityChain(resolved)
	if err != nil {
		return fail(nil, err)
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return fail(nil, err)
	}
	lk := &Lock{}
	if err := lk.lockIdentities(
		registry, ancestorIdentities, len(missing) == 0, target); err != nil {
		return fail(lk, err)
	}
	slices.Reverse(missing)
	for i, component := range missing {
		next, enterErr := enterVaultDir(root, component)
		_ = root.Close()
		if enterErr != nil {
			root = nil
			_ = lk.Release()
			return nil, nil, fmt.Errorf(
				"creating vault root component %q: %w", component, enterErr)
		}
		root = next
		if afterOpen != nil {
			if err := afterOpen(i, root); err != nil {
				return fail(lk, err)
			}
		}
		info, err := root.Stat(".")
		if err != nil {
			return fail(lk, fmt.Errorf(
				"checking created vault root component %q: %w", component, err))
		}
		identityPath := filepath.Join(append([]string{resolved}, missing[:i+1]...)...)
		identity, err := directoryIdentity(info, identityPath)
		if err != nil {
			return fail(lk, err)
		}
		if err := lk.lockIdentities(
			registry, []string{identity}, i == len(missing)-1, target); err != nil {
			return fail(lk, err)
		}
	}
	if err := verifyLayoutRoot(target, root); err != nil {
		return fail(lk, err)
	}
	local, err := openRootLock(root)
	if err != nil {
		return fail(lk, err)
	}
	if err := lockFile(local, true); err != nil {
		_ = local.Close()
		return fail(lk, classifyLockError(err, target))
	}
	lk.files = append(lk.files, local)
	if err := verifyLayoutRoot(target, root); err != nil {
		return fail(lk, err)
	}
	return root, lk, nil
}

func (lk *Lock) lockIdentities(
	registry string,
	identities []string,
	exclusiveFinal bool,
	target string,
) error {
	for i, identity := range identities {
		exclusive := exclusiveFinal && i == len(identities)-1
		f, err := openRegistryLock(registry, identity+".lock")
		if err != nil {
			return fmt.Errorf("opening target-tree lock: %w", err)
		}
		if err := lockFile(f, exclusive); err != nil {
			_ = f.Close()
			return classifyLockError(err, target)
		}
		lk.files = append(lk.files, f)
	}
	return nil
}

func (l Layout) tryLockOwnershipAcquisition() (*Lock, error) {
	target, err := CanonicalRoot(l.Root)
	if err != nil {
		return nil, err
	}
	current := target
	for {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("checking vault acquisition ancestor: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("vault root has no existing ancestor: %s", target)
		}
		current = parent
	}
	before, err := os.Stat(current)
	if err != nil {
		return nil, fmt.Errorf("checking vault acquisition ancestor identity: %w", err)
	}
	identities, err := directoryIdentityChain(current)
	if err != nil {
		return nil, err
	}
	registry, err := targetLockRegistryDir()
	if err != nil {
		return nil, err
	}
	lock := &Lock{}
	if err := lock.lockAcquisitionIdentities(
		registry, identities, true, target); err != nil {
		_ = lock.Release()
		return nil, err
	}
	after, err := os.Stat(current)
	if err != nil || !os.SameFile(before, after) {
		_ = lock.Release()
		if err != nil {
			return nil, fmt.Errorf("rechecking vault acquisition ancestor identity: %w", err)
		}
		return nil, fmt.Errorf("vault acquisition ancestor %s changed while locking", current)
	}
	return lock, nil
}

func (lk *Lock) lockAcquisitionIdentities(
	registry string,
	identities []string,
	exclusiveFinal bool,
	target string,
) error {
	for i, identity := range identities {
		exclusive := exclusiveFinal && i == len(identities)-1
		f, err := openRegistryLock(registry, "acquisition-"+identity+".lock")
		if err != nil {
			return err
		}
		if err := lockFile(f, exclusive); err != nil {
			_ = f.Close()
			return classifyLockError(err, target)
		}
		lk.files = append(lk.files, f)
	}
	return nil
}

func enterVaultDir(parent *os.Root, component string) (*os.Root, error) {
	info, err := parent.Lstat(component)
	if errors.Is(err, os.ErrNotExist) {
		if err := mkdirVaultDir(parent, component); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat(component)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("path component %q is not a real directory", component)
	}
	root, err := parent.OpenRoot(component)
	if err != nil {
		return nil, err
	}
	held, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(info, held) {
		_ = root.Close()
		return nil, fmt.Errorf("path component %q changed while opening", component)
	}
	return root, nil
}

func verifyLayoutRoot(path string, root *os.Root) error {
	byPath, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking vault root %s: %w", path, err)
	}
	byRoot, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("checking held vault root: %w", err)
	}
	if !os.SameFile(byPath, byRoot) {
		return fmt.Errorf("vault root %s was replaced while locking", path)
	}
	return nil
}

func directoryIdentityChain(target string) ([]string, error) {
	var paths []string
	for current := filepath.Clean(target); ; current = filepath.Dir(current) {
		paths = append(paths, current)
		if current == filepath.Dir(current) {
			break
		}
	}
	slices.Reverse(paths)
	identities := make([]string, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("checking target-tree ancestor %s: %w", path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("target-tree ancestor %s is not a directory", path)
		}
		identity, err := directoryIdentity(info, path)
		if err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

// targetLockRegistryTestBase is set only by same-package tests that exercise
// the process-global launch mutex. Package test binaries run concurrently, so
// those unit tests must not contend with cmd/docbank's real daemon processes.
var targetLockRegistryTestBase string

func targetLockRegistryDir() (string, error) {
	dir := targetLockRegistryTestBase
	if dir == "" {
		var err error
		dir, err = targetLockRegistryBase()
		if err != nil {
			return "", err
		}
	}
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("securing target-lock registry: %w", err)
	}
	return dir, nil
}

func openRegistryLock(registry, name string) (*os.File, error) {
	path := filepath.Join(registry, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening target-tree lock: %w", err)
	}
	opened, statErr := f.Stat()
	leaf, lstatErr := os.Lstat(path)
	if err := errors.Join(statErr, lstatErr); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("checking target-tree lock: %w", err)
	}
	if !opened.Mode().IsRegular() || !leaf.Mode().IsRegular() ||
		!os.SameFile(opened, leaf) {
		_ = f.Close()
		return nil, errors.New("target-tree lock must be one stable regular file")
	}
	return f, nil
}

func openRootLock(root *os.Root) (*os.File, error) {
	f, err := root.OpenFile("vault.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening vault lock: %w", err)
	}
	opened, statErr := f.Stat()
	leaf, lstatErr := root.Lstat("vault.lock")
	if err := errors.Join(statErr, lstatErr); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("checking vault lock: %w", err)
	}
	if !opened.Mode().IsRegular() || !leaf.Mode().IsRegular() ||
		!os.SameFile(opened, leaf) {
		_ = f.Close()
		return nil, errors.New("vault.lock must be one stable regular file")
	}
	return f, nil
}

func classifyLockError(err error, target string) error {
	if errors.Is(err, errLockWouldBlock) {
		return fmt.Errorf("%s: %w (is a docbank daemon or restore already running?)",
			target, ErrVaultLocked)
	}
	return fmt.Errorf("locking vault target tree: %w", err)
}

func (lk *Lock) relockAt(index int, exclusive bool, target string) error {
	if lk == nil || index < 0 || index >= len(lk.files) {
		return errors.New("vault lock hierarchy entry is unavailable")
	}
	f := lk.files[index]
	if err := unlockFile(f); err != nil {
		return err
	}
	if err := lockFile(f, exclusive); err != nil {
		closeErr := f.Close()
		lk.files = append(lk.files[:index], lk.files[index+1:]...)
		return errors.Join(classifyLockError(err, target), closeErr)
	}
	return nil
}

func (lk *Lock) releaseFrom(index int) error {
	if lk == nil || index < 0 || index > len(lk.files) {
		return errors.New("vault lock release boundary is invalid")
	}
	files := lk.files[index:]
	lk.files = lk.files[:index]
	var errs []error
	for _, f := range slices.Backward(files) {
		if err := unlockFile(f); err != nil {
			errs = append(errs, err)
		}
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("releasing vault lock: %w", err)
	}
	return nil
}

// Release drops the lock.
func (lk *Lock) Release() error {
	if lk == nil {
		return nil
	}
	return lk.releaseFrom(0)
}
