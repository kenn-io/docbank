package filepublish

import (
	"os"
	"unsafe"

	"go.kenn.io/docbank/internal/winsecurity"
	"golang.org/x/sys/windows"
)

func makePrivateRootStage(parent *os.Root, name string) (*os.File, error) {
	return winsecurity.MkdirPrivatePinnedAt(parent, name)
}

func syncRootDirectory(_ *os.Root, _ string) error { return nil }

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(source.Fd()), ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.SYNCHRONIZE, attributes, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	encoded, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	type renameInformation struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [1]uint16
	}
	var layout renameInformation
	length := (len(encoded) - 1) * 2
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+length)
	info := (*renameInformation)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = windows.Handle(destination.Fd())
	info.FileNameLength = uint32(length)
	copy(unsafe.Slice(&info.FileName[0], len(encoded)-1), encoded)
	return windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
}
