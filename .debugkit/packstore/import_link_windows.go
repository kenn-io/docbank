//go:build windows

package packstore

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isImportLinkUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) ||
		errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}
