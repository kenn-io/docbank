//go:build windows

package packstore

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func snapshotPathIdentity(path string) (result fs.FileInfo, resultErr error) {
	f, err := openWindowsNoFollow(path, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			result = nil
			resultErr = errors.Join(resultErr, err)
		}
	}()
	return f.Stat()
}

func openNoFollow(path string, durable bool) (*os.File, error) {
	access := uint32(windows.GENERIC_READ)
	if durable {
		access |= windows.GENERIC_WRITE
	}
	return openWindowsNoFollow(path, access)
}

// openLooseRepairPin holds only an identity handle. Zero requested access is
// deliberate: ReplaceFileW opens the replacement without sharing, so a readable
// pin would conflict even if that pin granted FILE_SHARE_DELETE.
func openLooseRepairPin(path string) (*os.File, fs.FileInfo, error) {
	f, err := openWindowsNoFollow(path, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	if err := validateRegularNoFollow(path, info); err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return f, info, nil
}

func openWindowsNoFollow(path string, access uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Identity snapshots must inspect foreign directories without traversing
	// them. Windows requires BACKUP_SEMANTICS to open a directory handle; it
	// does not enable backup-privilege bypass on its own.
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|
			windows.FILE_FLAG_BACKUP_SEMANTICS|
			windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("packstore: create file for Windows handle")
	}
	return f, nil
}

func validatePlatformFileInfo(info fs.FileInfo) error {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return errors.New("cannot inspect Windows file attributes")
	}
	if attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("is a Windows reparse point")
	}
	return nil
}
