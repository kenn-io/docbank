//go:build !unix

package home

import (
	"errors"
	"os"
)

var errUnsupported = errors.New(
	"docbank requires a Unix-like OS: vault locking uses flock(2)")

// Lock is a held advisory lock on the vault (unsupported on this platform).
type Lock struct{}

// ErrVaultLocked is returned by TryLockExclusive when another process
// already holds the vault lock (unsupported on this platform).
var ErrVaultLocked = errors.New("vault is locked by another process")

// TryLockExclusive fails: vault locking is Unix-only.
func (l Layout) TryLockExclusive() (*Lock, error) { return nil, errUnsupported }

// TryLockExclusiveRoot fails: vault locking is Unix-only.
func (l Layout) TryLockExclusiveRoot(*os.Root) (*Lock, error) { return nil, errUnsupported }

// TryLockRestoreRoot fails: vault locking is Unix-only.
func (l Layout) TryLockRestoreRoot(*os.Root) (*Lock, error) { return nil, errUnsupported }

// Release fails: vault locking is Unix-only.
func (lk *Lock) Release() error { return errUnsupported }
