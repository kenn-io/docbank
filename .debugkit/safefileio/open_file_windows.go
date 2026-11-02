//go:build windows

package safefileio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

var reOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// OpenCurrentUserFile opens path without following reparse points and verifies
// the opened handle is a regular file owned by the current token user or token
// owner.
func OpenCurrentUserFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	path16, err := winpath.UTF16Ptr(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = windows.CloseHandle(handle)
		}
	}()
	if err := validateWindowsFileHandle(path, handle); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	success = true
	return file, nil
}

// ValidateCurrentUserFile verifies an already-open handle is a regular,
// non-reparse file owned by the current token user or token owner.
func ValidateCurrentUserFile(file *os.File) error {
	if file == nil {
		return errors.New("file is nil")
	}
	return validateWindowsFileHandle(file.Name(), windows.Handle(file.Fd()))
}

// ValidatePrivateCurrentUserFile verifies that an open current-user-owned file
// has a protected DACL granting access only to the current user and Windows
// administrative principals.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	handle, err := reopenWindowsFileForDACL(file)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return err
	}
	return verifyWindowsFileDACL(file.Name(), handle, userSID, ownerSID)
}

func reopenWindowsFileForDACL(file *os.File) (windows.Handle, error) {
	if err := ValidateCurrentUserFile(file); err != nil {
		return 0, err
	}
	result, _, callErr := reOpenFile.Call(
		file.Fd(),
		uintptr(windows.READ_CONTROL),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		0,
	)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if !errors.Is(callErr, windows.ERROR_SUCCESS) {
			return 0, callErr
		}
		return 0, windows.ERROR_INVALID_HANDLE
	}
	if err := validateWindowsFileHandle(file.Name(), handle); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func validateWindowsFileHandle(path string, handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("%s is a directory", path)
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil {
		return fmt.Errorf("%s owner is missing", path)
	}
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return err
	}
	if !windowsOwnerMatches(owner, userSID, ownerSID) {
		return fmt.Errorf("%s is not owned by current user or token owner", path)
	}
	return nil
}
