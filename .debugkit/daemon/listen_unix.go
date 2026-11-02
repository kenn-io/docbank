//go:build !windows

package daemon

import (
	"errors"
	"syscall"
)

func isStaleUnixSocketDialError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}
