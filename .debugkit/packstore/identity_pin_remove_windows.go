//go:build windows

package packstore

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func removeFileIdentityPinClaim(file *os.File, _ string) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	err := windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&flags)),
		uint32(unsafe.Sizeof(flags)),
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_INVALID_FUNCTION) &&
		!errors.Is(err, windows.ERROR_NOT_SUPPORTED) &&
		!errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return err
	}

	// Older Windows versions and filesystems may not implement POSIX
	// disposition. Retain the exact-handle legacy fallback rather than
	// reopening the claimed pathname; active readers can defer its final
	// deletion, which the caller reports if the private claim stays non-empty.
	deleteFile := byte(1)
	return windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileDispositionInfo,
		&deleteFile,
		1,
	)
}
