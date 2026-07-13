// Vault locking requires a Unix-like OS (flock); docbank does not support
// Windows.

//go:build unix

package home

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
)

// LockPath is the advisory lock file serializing vault access across
// processes: the daemon holds it exclusively for its entire lifetime.
func (l Layout) LockPath() string { return filepath.Join(l.Root, "vault.lock") }

// Lock is a held advisory lock on the vault.
type Lock struct {
	files []*os.File
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err //nolint:wrapcheck // raw errno needed: TryLockExclusive matches EWOULDBLOCK; exported callers wrap
		}
	}
}

// ErrVaultLocked is returned by TryLockExclusive when another process
// already holds the vault lock.
var ErrVaultLocked = errors.New("vault is locked by another process")

// TryLockExclusive takes the vault lock without blocking. The daemon is the
// single lock holder for the vault's lifetime; a second daemon (or a stale
// holder) surfaces immediately instead of hanging.
func (l Layout) TryLockExclusive() (*Lock, error) {
	root, err := os.OpenRoot(l.Root)
	if err != nil {
		return nil, fmt.Errorf("opening vault root %s: %w", l.Root, err)
	}
	defer func() { _ = root.Close() }()
	return l.TryLockExclusiveRoot(root)
}

// OpenAndLockExclusive opens an existing vault root or creates a missing one
// through a held parent while coordinating every existing ancestor. The
// returned root is the same directory protected by the returned lock.
func (l Layout) OpenAndLockExclusive() (*os.Root, *Lock, error) {
	root, err := os.OpenRoot(l.Root)
	if err == nil {
		lk, lockErr := l.TryLockExclusiveRoot(root)
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
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
	if err := flock(local, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
		identity, err := directoryIdentity(info, component)
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
	if err := flock(local, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
		how := syscall.LOCK_SH | syscall.LOCK_NB
		if exclusiveFinal && i == len(identities)-1 {
			how = syscall.LOCK_EX | syscall.LOCK_NB
		}
		f, err := openRegistryLock(registry, identity+".lock")
		if err != nil {
			return fmt.Errorf("opening target-tree lock: %w", err)
		}
		if err := flock(f, how); err != nil {
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
		if err := parent.Mkdir(component, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
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

func directoryIdentity(info os.FileInfo, path string) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("reading target-tree identity for %s", path)
	}
	return fmt.Sprintf("dev-%x-ino-%x", stat.Dev, stat.Ino), nil
}

func targetLockRegistryDir() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return "", fmt.Errorf("resolving effective user: %w", err)
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", fmt.Errorf("effective user home is not absolute: %q", account.HomeDir)
	}
	dir := filepath.Join(account.HomeDir, ".local", "state", "docbank", "target-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating target-lock registry: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("checking target-lock registry: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ok ||
		strconv.FormatUint(uint64(stat.Uid), 10) != account.Uid {
		return "", errors.New("target-lock registry must be a private directory owned by the effective user")
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs owner execute permission
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
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("%s: %w (is a docbank daemon or restore already running?)",
			target, ErrVaultLocked)
	}
	return fmt.Errorf("locking vault target tree: %w", err)
}

// Release drops the lock.
func (lk *Lock) Release() error {
	var errs []error
	for _, f := range slices.Backward(lk.files) {
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	lk.files = nil
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("releasing vault lock: %w", err)
	}
	return nil
}
