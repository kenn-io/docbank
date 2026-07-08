// Vault locking requires a Unix-like OS (flock); docbank does not support
// Windows.

//go:build unix

package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockPath is the advisory lock file serializing vault access across
// processes: the daemon holds it exclusively for its entire lifetime.
func (l Layout) LockPath() string { return filepath.Join(l.Root, "vault.lock") }

// Lock is a held advisory lock on the vault.
type Lock struct {
	f *os.File
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
	f, err := os.OpenFile(l.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening vault lock %s: %w", l.LockPath(), err)
	}
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w (is a docbank daemon already running?)", l.LockPath(), ErrVaultLocked)
		}
		return nil, fmt.Errorf("locking vault: %w", err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (lk *Lock) Release() error {
	if err := lk.f.Close(); err != nil {
		return fmt.Errorf("releasing vault lock: %w", err)
	}
	return nil
}
