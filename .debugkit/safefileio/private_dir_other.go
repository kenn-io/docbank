//go:build !unix && !windows

package safefileio

import (
	"fmt"
	"runtime"
)

// EnsurePrivateDir fails closed when ownership and link-safe directory
// handling are unavailable.
func EnsurePrivateDir(string) error {
	return fmt.Errorf(
		"safefileio: private directory creation is unsupported on %s",
		runtime.GOOS,
	)
}

// EnsurePrivateDirShallow fails closed like EnsurePrivateDir.
func EnsurePrivateDirShallow(path string) error {
	return EnsurePrivateDir(path)
}

// ValidatePrivateDir fails closed when ownership and link-safe directory
// validation are unavailable.
func ValidatePrivateDir(string) error {
	return fmt.Errorf(
		"safefileio: private directory validation is unsupported on %s",
		runtime.GOOS,
	)
}
