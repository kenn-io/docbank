package qmdexport

import (
	"errors"
	"os"
	"strings"
	"unsafe"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func nativeVolumeRoot(volume string) (*anchoredDir, error) {
	// Permit ordinary drive and UNC volume roots only, never device namespaces.
	if strings.HasPrefix(volume, `\\?\`) || strings.HasPrefix(volume, `\\.\`) {
		return nil, errEntry
	}
	path, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return nil, errEntry
	}
	handle, err := windows.CreateFile(path, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	f := os.NewFile(uintptr(handle), "qmd directory")
	if err := nativeType(f, true); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &anchoredDir{file: f}, nil
}

func nativeType(file *os.File, directory bool) error {
	handle := windows.Handle(file.Fd())
	typeID, err := windows.GetFileType(handle)
	if err != nil {
		return nativeError(err)
	}
	if typeID != windows.FILE_TYPE_DISK {
		return errEntry
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nativeError(err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errEntry
	}
	return nil
}

func nativeOpen(d *anchoredDir, name string, directory, pinned bool) (*os.File, error) {
	return nativeOpenAccess(d, name, directory, pinned, false)
}

func nativeOpenAccess(d *anchoredDir, name string, directory, pinned, remove bool) (*os.File, error) {
	component, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, errEntry
	}
	attributes := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(d.file.Fd()), ObjectName: component,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	access := uint32(windows.FILE_GENERIC_READ | windows.READ_CONTROL | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	sharing := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if !pinned {
		sharing |= windows.FILE_SHARE_DELETE
	}
	if remove {
		access |= windows.DELETE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access, attributes, &status, nil, 0, sharing, windows.FILE_OPEN, options, 0, 0)
	if err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) {
			return nil, os.ErrNotExist
		}
		return nil, nativeError(err)
	}
	f := os.NewFile(uintptr(handle), "qmd entry")
	if err := nativeType(f, directory); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func nativeReopenDirectory(d *anchoredDir) (*os.File, error) { return nativeOpen(d, ".", true, false) }

func nativeCreate(d *anchoredDir, name string, directory bool, role entryRole) (*os.File, error) {
	if directory {
		if role == stableEntry {
			return winsecurity.MkdirPrivatePinnedFileAt(d.file, name)
		}
		return winsecurity.MkdirPrivateMovableFileAt(d.file, name)
	}
	if role == stableEntry {
		return winsecurity.CreatePrivatePinnedFileAt(d.file, name)
	}
	return winsecurity.CreatePrivateMovableFileAt(d.file, name)
}

func validatePrivateEntry(file *os.File, directory bool) error {
	if directory {
		if err := winsecurity.ValidatePrivateDirectoryFile(file); err != nil {
			return errPrivate
		}
	} else {
		if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
			return errPrivate
		}
	}
	return nil
}

func nativeRemove(d *anchoredDir, name string, identity entryIdentity, directory bool) (err error) {
	f, err := nativeOpenAccess(d, name, directory, false, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := heldIdentity(f, identity); err != nil {
		return err
	}
	// Class 64 and DELETE|POSIX_SEMANTICS remove this exact handle's entry.
	// Unsupported filesystems fail; no pathname fallback or permission repair.
	flags := uint32(0x1 | 0x2)
	var status windows.IO_STATUS_BLOCK
	// #nosec G103 -- Class 64 consumes exactly one uint32 flag word, held through the synchronous native call.
	return nativeError(windows.NtSetInformationFile(windows.Handle(f.Fd()), &status, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)), windows.FileDispositionInformationEx))
}

func nativeMove(sourceParent *anchoredDir, name string, source *os.File, targetParent *anchoredDir, target string, replace bool) error {
	// Match Go 1.27's FILE_RENAME_INFORMATION_EX layout and actual class 65.
	// No classic-class or remove-first fallback weakens this contract.
	type renameInformationEx struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [260]uint16
	}
	encoded, err := windows.UTF16FromString(target)
	if err != nil || len(encoded) < 1 || len(encoded) > 260 {
		return errEntry
	}
	// #nosec G115 -- UTF-16 length is bounded to 1..260 code units above.
	info := renameInformationEx{RootDirectory: windows.Handle(targetParent.file.Fd()), FileNameLength: uint32((len(encoded) - 1) * 2)}
	if replace {
		info.Flags = 0x1 | 0x2
	}
	copy(info.FileName[:], encoded)
	var status windows.IO_STATUS_BLOCK
	// #nosec G103 -- This matches Go's class-65 native structure layout; the full buffer remains live for this synchronous call.
	return nativeError(windows.NtSetInformationFile(windows.Handle(source.Fd()), &status, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), 65))
}

func nativeTryLock(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY|windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLockBusy
	}
	return nativeError(err)
}

func nativeUnlock(file *os.File) error {
	return nativeError(windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{}))
}

// Initialization has already flushed its writable marker. Flush the retained
// writable bootstrap lock too; Windows does not claim Unix directory fsync.
func confirmInitialization(r *ownedRoot) error { return r.lock.Sync() }
