//go:build windows

package store

import (
	"errors"
	"os"
	"syscall"
)

const (
	errWindowsSharingViolation = syscall.Errno(32)
	errWindowsLockViolation    = syscall.Errno(33)
)

// isTemporaryWriteSharingError reports whether openErr represents a transient
// Windows sharing or lock restriction on a file that is not marked read-only.
func isTemporaryWriteSharingError(path string, openErr error) bool {
	if !errors.Is(openErr, errWindowsSharingViolation) && !errors.Is(openErr, errWindowsLockViolation) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&0o200 != 0
}
