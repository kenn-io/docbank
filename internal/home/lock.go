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
	"strings"
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

// TryLockExclusiveRoot coordinates an exclusive vault owner against the exact
// borrowed directory descriptor it will mutate. Shared locks on every ancestor
// identity make an owner conflict with restores of parent or descendant trees;
// the target's ordinary vault.lock preserves coordination with older builds.
func (l Layout) TryLockExclusiveRoot(root *os.Root) (*Lock, error) {
	return l.tryLockExclusiveRoot(root, false)
}

// TryLockRestoreRoot takes exclusive per-user restore ownership in addition to
// the vault-tree locks. Serializing restores closes the otherwise unresolvable
// Unix rename case where a held target is reparented after its ancestor
// identities were locked. Daemon startup checks the same ownership file but
// releases that gate once its stable tree lock is established.
func (l Layout) TryLockRestoreRoot(root *os.Root) (*Lock, error) {
	return l.tryLockExclusiveRoot(root, true)
}

func (l Layout) tryLockExclusiveRoot(root *os.Root, restore bool) (*Lock, error) {
	target, err := filepath.Abs(l.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving vault root %s: %w", l.Root, err)
	}
	if err := verifyLayoutRoot(target, root); err != nil {
		return nil, err
	}
	identities, err := directoryIdentityChain(target)
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
	global, err := openRegistryLock(registry, "restore-owner.lock")
	if err != nil {
		return nil, err
	}
	globalHow := syscall.LOCK_SH | syscall.LOCK_NB
	if restore {
		globalHow = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := flock(global, globalHow); err != nil {
		_ = global.Close()
		return nil, classifyLockError(err, target)
	}
	lk.files = append(lk.files, global)
	for i, identity := range identities {
		how := syscall.LOCK_SH | syscall.LOCK_NB
		if i == len(identities)-1 {
			how = syscall.LOCK_EX | syscall.LOCK_NB
		}
		f, openErr := openRegistryLock(registry, identity+".lock")
		if openErr != nil {
			return nil, fmt.Errorf("opening target-tree lock: %w", openErr)
		}
		if lockErr := flock(f, how); lockErr != nil {
			_ = f.Close()
			return nil, classifyLockError(lockErr, target)
		}
		lk.files = append(lk.files, f)
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
	if !restore {
		if err := global.Close(); err != nil {
			return nil, fmt.Errorf("releasing restore-startup gate: %w", err)
		}
		lk.files = slices.Delete(lk.files, 0, 1)
	}
	failed = false
	return lk, nil
}

func verifyLayoutRoot(path string, root *os.Root) error {
	byPath, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checking vault root %s: %w", path, err)
	}
	if byPath.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("vault root %s was replaced with a symlink", path)
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
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("reading target-tree identity for %s", path)
		}
		identities = append(identities,
			fmt.Sprintf("dev-%x-ino-%x", stat.Dev, stat.Ino))
	}
	return identities, nil
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
	// Separate go test package binaries deliberately exercise independent
	// temporary vaults in parallel. Giving each test process its own registry
	// preserves in-process lock semantics without making unrelated package
	// suites behave like competing production restores.
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		dir = filepath.Join(dir, "tests", strconv.Itoa(os.Getpid()))
	}
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
