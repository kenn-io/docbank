package filepublish

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"go.kenn.io/docbank/internal/winsecurity"
	"golang.org/x/sys/windows"
)

func createPrivateRootFile(root *os.Root, name string) (*os.File, error) {
	return winsecurity.CreatePrivateFileAt(root, name)
}

func syncRootDirectory(_ *os.Root, _ string) error { return nil }

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return fmt.Errorf("encoding checkout stage name: %w", err)
	}
	attributes := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(source.Fd()), ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.SYNCHRONIZE, attributes, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return fmt.Errorf("opening checkout stage for publication: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	encoded, err := windows.UTF16FromString(target)
	if err != nil {
		return fmt.Errorf("encoding checkout filename: %w", err)
	}
	type renameInformation struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [1]uint16
	}
	var layout renameInformation
	length := (len(encoded) - 1) * 2
	if length < 0 || length > 65534 {
		return fmt.Errorf("checkout filename exceeds the Windows Unicode limit: %w", os.ErrInvalid)
	}
	nameLength := uint32(length)
	bufferSize := uint32(unsafe.Offsetof(layout.FileName)) + nameLength
	buffer := make([]byte, bufferSize)
	info := (*renameInformation)(unsafe.Pointer(&buffer[0])) // #nosec G103 -- The buffer includes the native header and the checked UTF-16 payload.
	info.RootDirectory = windows.Handle(destination.Fd())
	info.FileNameLength = nameLength
	copy(unsafe.Slice(&info.FileName[0], len(encoded)-1), encoded) // #nosec G103 -- The slice covers exactly the allocated UTF-16 payload.
	if err := windows.NtSetInformationFile(handle, &status, &buffer[0], bufferSize, windows.FileRenameInformation); err != nil {
		return fmt.Errorf("publishing checkout file without replacement: %w", err)
	}
	return nil
}

func createPrivateRootDirectory(root *os.Root, name string) error {
	parent, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	return winsecurity.MkdirPrivateAt(parent, filepath.Base(name))
}
