//go:build !unix && !windows

package safefileio

import (
	"fmt"
	"os"
	"runtime"
)

// OpenCurrentUserFile fails closed when the platform cannot bind an open file
// to a verified current-user identity without following links.
func OpenCurrentUserFile(string) (*os.File, error) {
	return nil, fmt.Errorf(
		"safefileio: current-user file opening is unsupported on %s",
		runtime.GOOS,
	)
}

// ValidateCurrentUserFile fails closed when handle ownership cannot be
// established on the current platform.
func ValidateCurrentUserFile(*os.File) error {
	return fmt.Errorf(
		"safefileio: current-user file validation is unsupported on %s",
		runtime.GOOS,
	)
}

// ValidatePrivateCurrentUserFile fails closed when the platform cannot verify
// current-user-only file access.
func ValidatePrivateCurrentUserFile(*os.File) error {
	return fmt.Errorf(
		"safefileio: private current-user file validation is unsupported on %s",
		runtime.GOOS,
	)
}

// CreatePrivateFile fails closed when the platform cannot create a file with
// verified current-user-only access.
func CreatePrivateFile(string) (*os.File, error) {
	return nil, fmt.Errorf(
		"safefileio: private file creation is unsupported on %s",
		runtime.GOOS,
	)
}
