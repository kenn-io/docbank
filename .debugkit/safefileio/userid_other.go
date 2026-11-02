//go:build !unix && !windows

package safefileio

import (
	"fmt"
	"runtime"
)

// CurrentUserID fails closed when the platform does not expose a stable
// filesystem user identity.
func CurrentUserID() (string, error) {
	return "", fmt.Errorf(
		"safefileio: current user identity is unsupported on %s",
		runtime.GOOS,
	)
}
