//go:build !windows

package processing

import (
	"errors"
	"syscall"
)

func mediaDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
