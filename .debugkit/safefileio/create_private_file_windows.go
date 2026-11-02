//go:build windows

package safefileio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

// fileDispositionInfo mirrors FILE_DISPOSITION_INFO.
type fileDispositionInfo struct {
	DeleteFile bool
}

// CreatePrivateFile creates a new regular file at path, opened read-write,
// whose protected DACL grants access only to the current user, LocalSystem,
// and built-in Administrators from the moment it exists. It fails with an
// error wrapping fs.ErrExist when anything, including a symlink or junction,
// already exists at path. It never repairs an existing file.
func CreatePrivateFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	path16, err := winpath.UTF16Ptr(path)
	if err != nil {
		return nil, err
	}
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return nil, err
	}
	attrs, err := privateFileSecurityAttributes(userSID)
	if err != nil {
		return nil, fmt.Errorf("build private security descriptor for %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		attrs,
		windows.CREATE_NEW,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		// ERROR_FILE_EXISTS and ERROR_ALREADY_EXISTS match fs.ErrExist. A
		// directory at path, including a directory junction or symlink,
		// fails with ERROR_ACCESS_DENIED instead; report that as existing.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if _, statErr := os.Lstat(path); statErr == nil {
				err = errors.Join(fs.ErrExist, err)
			}
		}
		return nil, &fs.PathError{Op: "create", Path: path, Err: err}
	}
	if err := verifyCreatedWindowsFile(path, handle, userSID, ownerSID); err != nil {
		return nil, errors.Join(err, discardCreatedWindowsFile(path, handle))
	}
	return os.NewFile(uintptr(handle), path), nil
}

func privateFileSecurityAttributes(userSID *windows.SID) (*windows.SecurityAttributes, error) {
	acl, err := privateWindowsACL(userSID, windows.NO_INHERITANCE)
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := descriptor.SetDACL(acl, true, false); err != nil {
		return nil, err
	}
	if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}, nil
}

func verifyCreatedWindowsFile(path string, handle windows.Handle, userSID, ownerSID *windows.SID) error {
	if err := validateWindowsFileHandle(path, handle); err != nil {
		return err
	}
	return verifyWindowsFileDACL(path, handle, userSID, ownerSID)
}

// discardCreatedWindowsFile marks the created file for deletion through its
// own handle, so a replaced path entry is never deleted, then closes it.
func discardCreatedWindowsFile(path string, handle windows.Handle) error {
	info := fileDispositionInfo{DeleteFile: true}
	deleteErr := windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if deleteErr != nil {
		deleteErr = fmt.Errorf("delete created file %s: %w", path, deleteErr)
	}
	return errors.Join(deleteErr, windows.CloseHandle(handle))
}
