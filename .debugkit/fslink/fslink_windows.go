//go:build windows

package fslink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

// reparseTagNameSurrogate is the IsReparseTagNameSurrogate bit: the reparse
// point names another file system entity instead of holding data in place.
const reparseTagNameSurrogate = 0x20000000

// symbolicLinkFlagAllowUnprivilegedCreate is
// SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE, which lets Developer Mode
// create symlinks without the privilege.
const symbolicLinkFlagAllowUnprivilegedCreate = 0x2

// fileAttributeTagInfo is FILE_ATTRIBUTE_TAG_INFO.
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

func (info fileAttributeTagInfo) isLink() bool {
	return info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 &&
		info.ReparseTag&reparseTagNameSurrogate != 0
}

func (info fileAttributeTagInfo) isDir() bool {
	return info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

func classify(path string) (Kind, error) {
	kind, err := classifyPath(path)
	if err != nil {
		return NotLink, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	return kind, nil
}

func classifyPath(path string) (Kind, error) {
	path16, err := winpath.UTF16Ptr(path)
	if err != nil {
		return NotLink, err
	}
	handle, err := windows.CreateFile(
		path16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return NotLink, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	info, disk, err := tagInfo(handle)
	if err != nil || !disk || !info.isLink() {
		return NotLink, err
	}
	switch info.ReparseTag {
	case windows.IO_REPARSE_TAG_SYMLINK:
		return Symlink, nil
	case windows.IO_REPARSE_TAG_MOUNT_POINT:
		// Directory junctions and volume mount points share this tag; only
		// the substitute name tells them apart.
		target, err := mountPointTarget(handle)
		if err != nil {
			return NotLink, err
		}
		if hasPrefixFold(target, `\??\Volume{`) {
			return OtherLink, nil
		}
		return Junction, nil
	default:
		return OtherLink, nil
	}
}

// tagInfo reads the attributes and reparse tag of an open handle. Handles to
// pipes and character devices report disk=false; they cannot be reparse
// points.
func tagInfo(handle windows.Handle) (info fileAttributeTagInfo, disk bool, err error) {
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return info, false, err
	}
	if fileType != windows.FILE_TYPE_DISK {
		return info, false, nil
	}
	err = windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		return info, true, err
	}
	return info, true, nil
}

// REPARSE_DATA_BUFFER layout for IO_REPARSE_TAG_MOUNT_POINT: an 8-byte
// header, four uint16 name offsets and lengths, then the UTF-16 path buffer.
const (
	reparseHeaderSize    = 8
	mountPointFieldsSize = 8
	mountPointPathOffset = reparseHeaderSize + mountPointFieldsSize
)

var errMalformedMountPoint = errors.New("fslink: malformed mount point reparse data")

func mountPointTarget(handle windows.Handle) (string, error) {
	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if err := windows.DeviceIoControl(
		handle,
		windows.FSCTL_GET_REPARSE_POINT,
		nil,
		0,
		&buf[0],
		uint32(len(buf)),
		&n,
		nil,
	); err != nil {
		return "", err
	}
	buf = buf[:n]
	if len(buf) < mountPointPathOffset {
		return "", errMalformedMountPoint
	}
	offset := int(binary.LittleEndian.Uint16(buf[8:]))
	length := int(binary.LittleEndian.Uint16(buf[10:]))
	start := mountPointPathOffset + offset
	if length%2 != 0 || start+length > len(buf) {
		return "", errMalformedMountPoint
	}
	name := make([]uint16, length/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(buf[start+2*i:])
	}
	return string(utf16.Decode(name)), nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func createJunction(target, link string) error {
	if err := makeJunction(target, link); err != nil {
		return &os.LinkError{Op: "junction", Old: target, New: link, Err: err}
	}
	return nil
}

func makeJunction(target, link string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("fslink: junction target is not a directory")
	}
	if err := requireLocalVolume(abs); err != nil {
		return err
	}
	buf, err := mountPointBuffer(abs)
	if err != nil {
		return err
	}
	absLink, err := filepath.Abs(link)
	if err != nil {
		return err
	}
	// Create the directory and keep the handle that created it, so the
	// reparse data and any rollback reach this directory and never whatever
	// else a concurrent rename puts at the same path.
	handle, err := createDir(filepath.Dir(absLink), filepath.Base(absLink))
	if err != nil {
		return err
	}
	if err := setReparsePoint(handle, buf); err != nil {
		return errors.Join(err, deleteAndClose(handle))
	}
	return windows.CloseHandle(handle)
}

// createDir creates the directory name inside parent and returns a handle to
// it. It fails with an error wrapping fs.ErrExist when any entry, links
// included, already has that name.
func createDir(parent, name string) (windows.Handle, error) {
	parent16, err := winpath.UTF16Ptr(parent)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// The relative create needs only a handle to the parent; the file system
	// checks FILE_ADD_SUBDIRECTORY against the parent's own DACL, so do not
	// ask for listing or read rights the caller may lack.
	dir, err := windows.CreateFile(
		parent16,
		windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	defer func() { _ = windows.CloseHandle(dir) }()
	return ntCreateAt(dir, name,
		windows.FILE_GENERIC_WRITE|windows.DELETE,
		0,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
	)
}

// deleteAndClose marks the file behind handle for deletion and closes it,
// removing exactly the entry handle names.
func deleteAndClose(handle windows.Handle) error {
	deleteFile := uint8(1) // FILE_DISPOSITION_INFO.DeleteFile
	err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &deleteFile, 1)
	return errors.Join(err, windows.CloseHandle(handle))
}

// requireLocalVolume rejects targets a junction cannot name: junctions must
// point at a local volume, not a network share.
func requireLocalVolume(abs string) error {
	root16, err := windows.UTF16PtrFromString(filepath.VolumeName(abs) + `\`)
	if err != nil {
		return err
	}
	switch windows.GetDriveType(root16) {
	case windows.DRIVE_UNKNOWN, windows.DRIVE_NO_ROOT_DIR, windows.DRIVE_REMOTE:
		return errors.New("fslink: junction target is not on a local volume")
	}
	return nil
}

// mountPointBuffer builds a directory-junction REPARSE_DATA_BUFFER naming abs.
func mountPointBuffer(abs string) ([]byte, error) {
	printName := strings.TrimPrefix(abs, `\\?\`)
	substitute, err := windows.UTF16FromString(`\??\` + printName)
	if err != nil {
		return nil, err
	}
	print16, err := windows.UTF16FromString(printName)
	if err != nil {
		return nil, err
	}
	// Both names keep their NUL terminators in the path buffer; the
	// recorded lengths exclude them.
	names := slices.Concat(substitute, print16)
	dataLen := mountPointFieldsSize + 2*len(names)
	if reparseHeaderSize+dataLen > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		return nil, errors.New("fslink: junction target path is too long")
	}
	substituteBytes := 2 * (len(substitute) - 1)
	buf := make([]byte, reparseHeaderSize+dataLen)
	binary.LittleEndian.PutUint32(buf[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:], uint16(dataLen))
	binary.LittleEndian.PutUint16(buf[8:], 0)
	binary.LittleEndian.PutUint16(buf[10:], uint16(substituteBytes))
	binary.LittleEndian.PutUint16(buf[12:], uint16(substituteBytes+2))
	binary.LittleEndian.PutUint16(buf[14:], uint16(2*(len(print16)-1)))
	for i, c := range names {
		binary.LittleEndian.PutUint16(buf[mountPointPathOffset+2*i:], c)
	}
	return buf, nil
}

func setReparsePoint(handle windows.Handle, buf []byte) error {
	var n uint32
	return windows.DeviceIoControl(
		handle,
		windows.FSCTL_SET_REPARSE_POINT,
		&buf[0],
		uint32(len(buf)),
		nil,
		0,
		&n,
		nil,
	)
}

func linkDir(target, link string) (Kind, error) {
	err := createDirSymlink(target, link)
	if err == nil {
		return Symlink, nil
	}
	if !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return NotLink, &os.LinkError{Op: "symlink", Old: target, New: link, Err: err}
	}
	if err := createJunction(junctionTarget(target, link), link); err != nil {
		return NotLink, err
	}
	return Junction, nil
}

// createDirSymlink always creates a directory symlink. os.Symlink picks a
// file symlink when target does not exist yet, which Windows then refuses to
// traverse once the directory appears.
func createDirSymlink(target, link string) error {
	target = filepath.FromSlash(target)
	link16, err := winpath.UTF16Ptr(link)
	if err != nil {
		return err
	}
	// As os.Symlink does, prefix only an absolute target: a relative one
	// would become absolute, and CreateSymbolicLink accepts long relative
	// targets as they are.
	convert := windows.UTF16PtrFromString
	if filepath.IsAbs(target) {
		convert = winpath.UTF16Ptr
	}
	target16, err := convert(target)
	if err != nil {
		return err
	}
	return windows.CreateSymbolicLink(link16, target16,
		windows.SYMBOLIC_LINK_FLAG_DIRECTORY|symbolicLinkFlagAllowUnprivilegedCreate)
}

// junctionTarget resolves target the way the symlink it replaces would have:
// a relative target against link's parent directory, and a rooted target
// without a volume (`\dir`) against link's volume rather than the working
// directory's.
func junctionTarget(target, link string) string {
	target = filepath.FromSlash(target)
	if filepath.VolumeName(target) != "" {
		return target
	}
	if target != "" && os.IsPathSeparator(target[0]) {
		return filepath.VolumeName(link) + target
	}
	return filepath.Join(filepath.Dir(link), target)
}

func openFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	handle, err := openHandle(path, flag, perm)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// openDirPath opens the directory at path without following a link in its
// final component.
func openDirPath(path string) (*os.File, error) {
	file, err := openFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.IsDir() {
		err = &fs.PathError{Op: "open", Path: path, Err: syscall.ENOTDIR}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openRegular(path string) (*os.File, error) {
	handle, err := openHandle(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	info, disk, err := tagInfo(handle)
	if err == nil {
		switch {
		case !disk:
			err = fmt.Errorf("%w: device or pipe", errNotRegular)
		case info.isDir():
			err = fmt.Errorf("%w: directory", errNotRegular)
		}
	}
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// openHandle mirrors syscall.Open's flag translation, but opens the final
// component itself rather than a reparse point's destination, rejects it if
// it is a link, and only then applies O_TRUNC.
func openHandle(path string, flag int, perm fs.FileMode) (windows.Handle, error) {
	if path == "" {
		return windows.InvalidHandle, windows.ERROR_FILE_NOT_FOUND
	}
	path16, err := winpath.UTF16Ptr(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	if flag&os.O_APPEND != 0 {
		// Keep FILE_WRITE_DATA only when O_TRUNC needs it; without it every
		// write appends.
		if flag&os.O_TRUNC == 0 {
			access &^= windows.GENERIC_WRITE
		}
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES |
			windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE
	}
	// The reparse tag check below needs attribute access whatever the mode.
	access |= windows.FILE_READ_ATTRIBUTES

	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	attrs |= windows.FILE_FLAG_BACKUP_SEMANTICS
	if flag&os.O_SYNC != 0 {
		attrs |= windows.FILE_FLAG_WRITE_THROUGH
	}
	// Never CREATE_ALWAYS or TRUNCATE_EXISTING: truncation waits until the
	// handle is known not to be a link.
	var disposition uint32
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disposition = windows.CREATE_NEW
	case flag&os.O_CREATE != 0:
		disposition = windows.OPEN_ALWAYS
	default:
		disposition = windows.OPEN_EXISTING
	}
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	handle, err := windows.CreateFile(path16, access, share, nil, disposition,
		attrs|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	info, disk, err := checkOpened(handle, flag)
	if err == nil && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		// A non-link reparse point (cloud placeholder, dedup, WOF) holds its
		// data behind a filter driver that FILE_FLAG_OPEN_REPARSE_POINT
		// bypasses. Reopen it normally and keep the new handle only if it
		// names the same file, so a link swapped in meanwhile is not
		// followed.
		handle, err = reopenVerified(handle, func() (windows.Handle, error) {
			return windows.CreateFile(path16, access, share, nil, windows.OPEN_EXISTING, attrs, 0)
		})
	}
	if err == nil && flag&os.O_TRUNC != 0 && disk && !info.isDir() {
		if flag&os.O_APPEND != 0 {
			handle, err = truncateToAppendOnly(handle, access&^windows.GENERIC_WRITE, share,
				attrs&(windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_WRITE_THROUGH))
		} else {
			err = windows.Ftruncate(handle, 0)
		}
	}
	if err != nil {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, err
	}
	return handle, nil
}

// checkOpened rejects a link, or a directory opened for writing, and
// returns the handle's attributes as tagInfo does.
func checkOpened(handle windows.Handle, flag int) (fileAttributeTagInfo, bool, error) {
	info, disk, err := tagInfo(handle)
	if err != nil || !disk {
		return info, disk, err
	}
	if info.isLink() {
		return info, disk, ErrIsLink
	}
	// FILE_FLAG_BACKUP_SEMANTICS lets directories open with write access;
	// refuse that the way Unix does.
	if info.isDir() && flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		return info, disk, syscall.EISDIR
	}
	return info, disk, nil
}

// reopenVerified opens a second handle with reopen and returns it if it names
// the same file as inspected. It always consumes inspected: on success
// inspected is closed, and on failure the returned handle is inspected so the
// caller closes it.
func reopenVerified(inspected windows.Handle, reopen func() (windows.Handle, error)) (windows.Handle, error) {
	want, err := handleID(inspected)
	if err != nil {
		return inspected, err
	}
	reopened, err := reopen()
	if err != nil {
		return inspected, err
	}
	got, err := handleID(reopened)
	if err == nil && got != want {
		err = errChanged
	}
	if err != nil {
		_ = windows.CloseHandle(reopened)
		return inspected, err
	}
	_ = windows.CloseHandle(inspected)
	return reopened, nil
}

// truncateToAppendOnly truncates the file behind handle and returns an
// append-only handle to it. Truncation needs FILE_WRITE_DATA, but a handle
// without it makes the OS append every write, even after a Seek, which
// os.NewFile cannot enforce because it does not know about O_APPEND. The
// append-only handle is opened and verified first, so a failure leaves the
// file untouched. Like reopenVerified it consumes handle: on success handle
// is closed, and on failure handle is returned for the caller to close.
func truncateToAppendOnly(handle windows.Handle, access, share, flags uint32) (windows.Handle, error) {
	want, err := handleID(handle)
	if err != nil {
		return handle, err
	}
	appendOnly, err := reOpenFile(handle, access, share, flags)
	if err != nil {
		return handle, err
	}
	got, err := handleID(appendOnly)
	if err == nil && got != want {
		err = errChanged
	}
	if err == nil {
		err = windows.Ftruncate(handle, 0)
	}
	if err != nil {
		_ = windows.CloseHandle(appendOnly)
		return handle, err
	}
	_ = windows.CloseHandle(handle)
	return appendOnly, nil
}

var procReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// reOpenFile calls ReOpenFile, which golang.org/x/sys/windows does not wrap.
func reOpenFile(handle windows.Handle, access, share, flags uint32) (windows.Handle, error) {
	r, _, err := procReOpenFile.Call(uintptr(handle), uintptr(access), uintptr(share), uintptr(flags))
	if windows.Handle(r) == windows.InvalidHandle {
		if errno, ok := errors.AsType[windows.Errno](err); ok && errno != 0 {
			return windows.InvalidHandle, errno
		}
		return windows.InvalidHandle, windows.ERROR_INVALID_HANDLE
	}
	return windows.Handle(r), nil
}

type fileID struct {
	volume, indexHigh, indexLow uint32
}

type entry struct {
	dir bool
	id  fileID
}

// lstatAt inspects name relative to dir's own handle, so no absolute path is
// reopened between the check and the caller's open through dir.
func lstatAt(dir *os.Root, name string) (entry, error) {
	parent, err := dir.Open(".")
	if err != nil {
		return entry{}, err
	}
	defer func() { _ = parent.Close() }()
	handle, err := ntCreateAt(windows.Handle(parent.Fd()), name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
	)
	if err != nil {
		return entry{}, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	info, _, err := tagInfo(handle)
	if err != nil {
		return entry{}, err
	}
	if info.isLink() {
		return entry{}, ErrIsLink
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		// os.Root opens every component with OBJ_DONT_REPARSE, so it would
		// refuse this entry anyway; say why instead of reporting ELOOP.
		return entry{}, errReparseInRoot
	}
	id, err := handleID(handle)
	if err != nil {
		return entry{}, err
	}
	return entry{dir: info.isDir(), id: id}, nil
}

// ntCreateAt calls NtCreateFile for name relative to the parent handle and
// maps a failing NTSTATUS to its Win32 error, so errors.Is sees fs.ErrExist
// and fs.ErrNotExist.
func ntCreateAt(parent windows.Handle, name string, access, share, disposition, options uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attrs := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		access|windows.SYNCHRONIZE,
		attrs,
		&windows.IO_STATUS_BLOCK{},
		nil,
		0,
		share,
		disposition,
		options,
		0,
		0,
	)
	if err != nil {
		if status, ok := errors.AsType[windows.NTStatus](err); ok {
			return windows.InvalidHandle, status.Errno()
		}
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func handleID(handle windows.Handle) (fileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fileID{}, err
	}
	return fileID{
		volume:    info.VolumeSerialNumber,
		indexHigh: info.FileIndexHigh,
		indexLow:  info.FileIndexLow,
	}, nil
}

func (e entry) isDir() bool {
	return e.dir
}

func (e entry) sameFile(file *os.File) (bool, error) {
	id, err := handleID(windows.Handle(file.Fd()))
	if err != nil {
		return false, err
	}
	return id == e.id, nil
}

func platformSupport() error { return nil }
