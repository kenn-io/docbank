//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

// RenameNoReplace atomically renames oldpath to newpath, failing with an
// error wrapping fs.ErrExist when newpath already exists. On Windows it uses
// MoveFileEx without MOVEFILE_REPLACE_EXISTING and without
// MOVEFILE_COPY_ALLOWED, so a cross-volume move fails instead of copying.
func RenameNoReplace(oldpath, newpath string) error {
	if err := moveFileEx(oldpath, newpath, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "MoveFileExW", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// Replace renames oldpath to newpath, atomically replacing an existing file
// at newpath. It never copies, so a rename across volumes fails. A symlink or
// junction at either path is renamed or replaced as an entry, never followed.
// A directory at newpath is never replaced: Replace fails with an error
// wrapping fs.ErrExist, as os.Rename does. That is a check just before the
// rename, not an atomic guarantee.
//
// On Windows it first renames with POSIX semantics (SetFileInformationByHandle
// with FileRenameInfoEx and FILE_RENAME_FLAG_POSIX_SEMANTICS). That succeeds
// while another handle holds newpath open with FILE_SHARE_DELETE, as os.Root
// and CreateFile with all three share flags open files; the holder keeps
// reading the old content. A handle opened without FILE_SHARE_DELETE, as
// os.Open opens files, still makes Replace fail with ERROR_SHARING_VIOLATION.
// When Windows or the file system reports that rename as unsupported, Replace
// falls back to MoveFileEx with MOVEFILE_REPLACE_EXISTING and
// MOVEFILE_WRITE_THROUGH, without MOVEFILE_COPY_ALLOWED, which fails while
// any other handle holds newpath open.
func Replace(oldpath, newpath string) error {
	if err := checkNotDirectory(oldpath, newpath); err != nil {
		return err
	}
	err := renamePOSIX(oldpath, newpath)
	if err == nil || !posixRenameUnsupported(err) {
		return err
	}
	if err := moveFileEx(oldpath, newpath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "MoveFileExW", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// checkNotDirectory refuses a directory at newpath the way os.Rename does
// on Unix, because POSIX semantics would let a directory replace an empty
// one. A junction or symlink is a link, not a directory, and os.Lstat does
// not report it as one. Like os.Rename it reports a missing oldpath first,
// and allows a case-only rename of a directory to itself.
func checkNotDirectory(oldpath, newpath string) error {
	// A missing or unreadable target is left for the rename to report.
	if fi, err := os.Lstat(newpath); err == nil && fi.IsDir() {
		ofi, err := os.Lstat(oldpath)
		if err != nil {
			if pathErr, ok := errors.AsType[*os.PathError](err); ok {
				err = pathErr.Err
			}
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
		}
		if newpath == oldpath || !os.SameFile(fi, ofi) {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: windows.ERROR_ALREADY_EXISTS}
		}
	}
	return nil
}

// setRenameInfo is SetFileInformationByHandle for the rename; tests replace
// it to exercise the fallback.
var setRenameInfo = windows.SetFileInformationByHandle

// posixRenameOp names the FileRenameInfoEx call in errors; posixRenameUnsupported
// only falls back on errors from that call, never from opening oldpath.
const posixRenameOp = "SetFileInformationByHandle"

// fileRenameInfo is FILE_RENAME_INFO from winbase.h, which x/sys/windows does
// not define. Flags is the union member used with FileRenameInfoEx; the
// FileName array runs past the end of the struct.
// https://learn.microsoft.com/windows/win32/api/winbase/ns-winbase-file_rename_info
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// renamePOSIX renames oldpath to newpath through a handle to oldpath with
// FILE_RENAME_FLAG_REPLACE_IF_EXISTS|FILE_RENAME_FLAG_POSIX_SEMANTICS.
//
// oldpath is opened with FILE_FLAG_OPEN_REPARSE_POINT so a link there is
// renamed rather than followed, as MoveFileEx does, and with
// FILE_FLAG_BACKUP_SEMANTICS so a directory can be opened. The handle asks
// only for DELETE and shares everything, so it conflicts with other handles
// no more than MoveFileEx does.
//
// FileRenameInfoEx has no write-through flag. The handle is opened with
// FILE_FLAG_WRITE_THROUGH instead, which is how MoveFileEx implements
// MOVEFILE_WRITE_THROUGH for a same-volume rename (it opens the source with
// FILE_WRITE_THROUGH). FlushFileBuffers is not used: it needs write access,
// which a read-only file or a directory would refuse.
//
// newpath goes in FileName as a Win32 path with RootDirectory NULL, which
// FILE_RENAME_INFO documents as accepting an absolute path or one relative to
// the current directory. It gets the same long-path handling as oldpath.
func renamePOSIX(oldpath, newpath string) error {
	oldPtr, err := winpath.UTF16Ptr(oldpath)
	if err != nil {
		return &os.LinkError{Op: "CreateFileW", Old: oldpath, New: newpath, Err: err}
	}
	newName, err := windows.UTF16FromString(winpath.Long(newpath))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	h, err := windows.CreateFile(oldPtr, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return &os.LinkError{Op: "CreateFileW", Old: oldpath, New: newpath, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()

	nameOff := unsafe.Offsetof(fileRenameInfo{}.FileName)
	size := nameOff + uintptr(len(newName))*2
	// Back the variable-length struct with uint64s so it is suitably aligned.
	buf := make([]uint64, (size+7)/8)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(newName) - 1) * 2) // without the NUL
	copy(unsafe.Slice(&info.FileName[0], len(newName)), newName)

	if err := setRenameInfo(h, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(info)), uint32(size)); err != nil {
		return &os.LinkError{Op: posixRenameOp, Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// posixRenameUnsupported reports whether err means the file system or
// Windows version rejects FileRenameInfoEx or POSIX semantics, rather than
// the rename itself failing. These are the codes Rust's std falls back on
// after probing Windows versions and file systems: ERROR_INVALID_PARAMETER
// before Windows 10 1607, ERROR_INVALID_FUNCTION on 1607, and
// ERROR_NOT_SUPPORTED on ReFS on Windows Server 2022. Access and sharing
// errors are never treated as unsupported: they report why this rename
// failed, and falling back would hide that.
func posixRenameUnsupported(err error) bool {
	linkErr, ok := errors.AsType[*os.LinkError](err)
	if !ok || linkErr.Op != posixRenameOp {
		return false
	}
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}

func moveFileEx(from, to string, flags uint32) error {
	fromPtr, err := winpath.UTF16Ptr(from)
	if err != nil {
		return err
	}
	toPtr, err := winpath.UTF16Ptr(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, flags)
}
