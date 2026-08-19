//go:build windows

package voyage

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fixtureFileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func renameFixtureDirectoryNoReplace(
	parentPath, stagingName, destinationName string,
	parentIdentity, stagingIdentity os.FileInfo,
) error {
	parent, err := openVerifiedWindowsFixtureParent(parentPath, parentIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	staging, err := openWindowsFixtureStaging(windows.Handle(parent.Fd()), stagingName, stagingIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = staging.Close() }()

	name, err := windows.UTF16FromString(destinationName)
	if err != nil {
		return fmt.Errorf("encode fixture destination name: %w", err)
	}
	nameBytes := (len(name) - 1) * 2
	var layout fixtureFileRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+nameBytes)
	information := (*fixtureFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.RootDirectory = windows.Handle(parent.Fd())
	information.FileNameLength = uint32(nameBytes) // #nosec G115 -- Windows paths are bounded far below uint32.
	copy(unsafe.Slice(&information.FileName[0], nameBytes/2), name[:len(name)-1])
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(
		windows.Handle(staging.Fd()), &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation, // #nosec G115 -- bounded path buffer.
	); err != nil {
		return fmt.Errorf("rename fixture directory without replacement: %w", err)
	}
	return nil
}

func openVerifiedWindowsFixtureParent(path string, identity os.FileInfo) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode fixture parent path: %w", err)
	}
	handle, err := windows.CreateFile(
		path16,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open fixture parent without following links: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("open fixture parent descriptor")
	}
	openedIdentity, err := file.Stat()
	if err != nil || !openedIdentity.IsDir() || !os.SameFile(identity, openedIdentity) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("fixture parent changed before no-replace publication"))
	}
	return file, nil
}

func openWindowsFixtureStaging(parent windows.Handle, name string, identity os.FileInfo) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, fmt.Errorf("encode fixture staging name: %w", err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		&attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	); err != nil {
		return nil, fmt.Errorf("open fixture staging directory relative to parent: %w", err)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("open fixture staging directory descriptor")
	}
	openedIdentity, err := file.Stat()
	if err != nil || !openedIdentity.IsDir() || !os.SameFile(identity, openedIdentity) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("fixture staging directory changed before no-replace publication"))
	}
	return file, nil
}

func openFixtureFileNoFollow(
	rootPath, name string,
	rootIdentity, fileIdentity os.FileInfo,
) (*os.File, error) {
	parent, err := openVerifiedWindowsFixtureParent(rootPath, rootIdentity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, fmt.Errorf("encode fixture file name: %w", err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ,
		&attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	); err != nil {
		return nil, fmt.Errorf("open fixture relative to pinned root: %w", err)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("open fixture descriptor")
	}
	openedIdentity, err := file.Stat()
	if err != nil || !openedIdentity.Mode().IsRegular() || !os.SameFile(fileIdentity, openedIdentity) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("fixture changed during descriptor-relative open"))
	}
	return file, nil
}
