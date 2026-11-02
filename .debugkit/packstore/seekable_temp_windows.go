//go:build windows

package packstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"go.kenn.io/kit/pack"
	"golang.org/x/sys/windows"
)

// createSeekableLooseTempPlatform asks Windows to delete the opened file
// object when its handle closes. Deletion is bound to the handle identity, so
// a later occupant of the same pathname is never removed by cleanup.
func createSeekableLooseTempPlatform() (*os.File, error) {
	security, err := seekableLooseTempSecurityAttributes()
	if err != nil {
		return nil, fmt.Errorf("packstore: create Windows seekable loose temporary security descriptor: %w", err)
	}
	const attempts = 8
	for range attempts {
		path := filepath.Join(os.TempDir(), "packstore-loose-open-"+pack.NewPackID())
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
			windows.FILE_SHARE_DELETE,
			security,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE,
			0,
		)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("packstore: create Windows seekable loose temporary file: %w", err)
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			return nil, errors.Join(
				fmt.Errorf("packstore: wrap Windows seekable loose temporary handle: %w", fs.ErrInvalid),
				windows.CloseHandle(handle),
			)
		}
		return file, nil
	}
	return nil, fmt.Errorf("packstore: create unique Windows seekable loose temporary file: %w", fs.ErrExist)
}

func seekableLooseTempSecurityAttributes() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	userSID := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + userSID + "D:P" +
			"(A;;GA;;;" + userSID + ")" +
			"(A;;GA;;;SY)" +
			"(A;;GA;;;BA)",
	)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}, nil
}
