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
// processes: normal commands hold it shared, gc holds it exclusively.
func (l Layout) LockPath() string { return filepath.Join(l.Root, "vault.lock") }

// Lock is a held advisory lock on the vault.
type Lock struct {
	f *os.File
}

// AcquireLock takes a shared (exclusive=false) or exclusive flock on the
// layout's lock file, blocking until it is available.
func (l Layout) AcquireLock(exclusive bool) (*Lock, error) {
	f, err := os.OpenFile(l.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening vault lock file: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := flock(f, how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking vault: %w", err)
	}
	return &Lock{f: f}, nil
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err //nolint:wrapcheck // raw errno needed: TryUpgrade matches EWOULDBLOCK; exported callers wrap
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

// TryUpgrade attempts a non-blocking conversion to an exclusive lock and
// reports whether it succeeded (false means another process holds the
// vault). Callers must Downgrade after their exclusive work.
func (lk *Lock) TryUpgrade() (bool, error) {
	err := flock(lk.f, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, fmt.Errorf("upgrading vault lock: %w", err)
	}
	// On Linux, flock conversions are release-then-acquire ("not guaranteed
	// to be atomic", flock(2)), so the failed upgrade may have dropped the
	// shared lock; take it back (blocking behind any exclusive holder that
	// slipped in) before reporting failure, or the caller would proceed
	// holding nothing. On Darwin the shared lock survives and this
	// reconverts shared to shared, a harmless no-op.
	if err := flock(lk.f, syscall.LOCK_SH); err != nil {
		return false, fmt.Errorf("reacquiring shared vault lock: %w", err)
	}
	return false, nil
}

// Downgrade converts the lock back to shared. The conversion may briefly
// release the lock, letting a waiting exclusive holder in first; callers
// must finish their exclusive work before downgrading.
func (lk *Lock) Downgrade() error {
	if err := flock(lk.f, syscall.LOCK_SH); err != nil {
		return fmt.Errorf("downgrading vault lock: %w", err)
	}
	return nil
}

// Release drops the lock.
func (lk *Lock) Release() error {
	if err := lk.f.Close(); err != nil {
		return fmt.Errorf("releasing vault lock: %w", err)
	}
	return nil
}
