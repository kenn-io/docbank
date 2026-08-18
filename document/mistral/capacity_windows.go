//go:build windows

package mistral

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isSpoolCapacityError(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) ||
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED)
}
