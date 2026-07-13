//go:build windows

package blob

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// OpenNoFollow opens a regular file without traversing a final-component
// reparse point. Sharing deletion is important for verified readers: an active
// stream retains its handle while pack maintenance or cleanup retires the
// directory entry.
func OpenNoFollow(path string) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encoding source path: %w", err)
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("opening source without following reparse points: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = windows.CloseHandle(handle)
		}
	}()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, fmt.Errorf("checking source handle: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, fmt.Errorf("source path %s is a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, fmt.Errorf("source path %s is not a regular file", path)
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return nil, fmt.Errorf("checking source file type: %w", err)
	}
	if fileType != windows.FILE_TYPE_DISK {
		return nil, fmt.Errorf("source path %s is not a regular disk file", path)
	}
	success = true
	return os.NewFile(uintptr(handle), path), nil
}
