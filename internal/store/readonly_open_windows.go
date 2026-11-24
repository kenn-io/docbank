//go:build windows

package store

import (
	"errors"
	"os"
	"syscall"
)

const (
	windowsSharingViolation = syscall.Errno(32)
	windowsLockViolation    = syscall.Errno(33)
)

// isTemporaryWriteSharingError reports whether openErr represents a transient
// Windows sharing or lock restriction on a file that is not marked read-only.
func isTemporaryWriteSharingError(path string, openErr error) bool {
	if !errors.Is(openErr, windowsSharingViolation) && !errors.Is(openErr, windowsLockViolation) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&0o200 != 0
}
