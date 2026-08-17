//go:build !windows

package mistral

import (
	"errors"
	"syscall"
)

func isSpoolCapacityError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
