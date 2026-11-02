//go:build windows

package daemon

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Windows reports WSAECONNREFUSED both for a socket file with no listener and
// for a missing path; neither maps to syscall.ECONNREFUSED.
func isStaleUnixSocketDialError(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
