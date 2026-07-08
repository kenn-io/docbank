//go:build !unix

package home

import "errors"

var errUnsupported = errors.New(
	"docbank requires a Unix-like OS: vault locking uses flock(2)")

// Lock is a held advisory lock on the vault (unsupported on this platform).
type Lock struct{}

// AcquireLock fails: vault locking is Unix-only.
func (l Layout) AcquireLock(bool) (*Lock, error) { return nil, errUnsupported }

// TryUpgrade fails: vault locking is Unix-only.
func (lk *Lock) TryUpgrade() (bool, error) { return false, errUnsupported }

// Downgrade fails: vault locking is Unix-only.
func (lk *Lock) Downgrade() error { return errUnsupported }

// Release fails: vault locking is Unix-only.
func (lk *Lock) Release() error { return errUnsupported }
