//go:build windows

package atomicfile_test

import (
	"errors"

	"golang.org/x/sys/windows"
)

func symlinkPrivilegeMissing(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}
