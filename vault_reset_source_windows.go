//go:build windows

package docbank

import (
	"fmt"
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
	"golang.org/x/sys/windows"
)

func resetSourceAttributesNoFollow(path string) (resetSourceAttributes, error) {
	extended, err := winsecurity.ExtendedLengthPath(path)
	if err != nil {
		return resetSourceAttributes{}, fmt.Errorf("resolving reset source path: %w", err)
	}
	path16, err := windows.UTF16PtrFromString(extended)
	if err != nil {
		return resetSourceAttributes{}, fmt.Errorf("encoding reset source path: %w", err)
	}
	handle, err := windows.CreateFile(
		path16,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return resetSourceAttributes{}, &os.PathError{
			Op:   "CreateFile",
			Path: path,
			Err:  err,
		}
	}
	defer windows.CloseHandle(handle)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return resetSourceAttributes{}, &os.PathError{
			Op:   "GetFileInformationByHandle",
			Path: path,
			Err:  err,
		}
	}
	return resetSourceAttributes{
		directory: info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
		reparse:   info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0,
	}, nil
}
