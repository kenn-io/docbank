//go:build windows

package winsecurity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MkdirPrivatePinnedFileAt creates a private directory with the original
// pinned-directory access and sharing policy, relative to a held parent.
func MkdirPrivatePinnedFileAt(parent *os.File, component string) (*os.File, error) {
	return createPrivateEntry(parent, component, true, false)
}

// MkdirPrivateMovableFileAt creates a directory whose own handle may rename it.
func MkdirPrivateMovableFileAt(parent *os.File, component string) (*os.File, error) {
	return createPrivateEntry(parent, component, true, true)
}

// CreatePrivatePinnedFileAt creates a private file without delete access or
// delete sharing, suitable for a permanent publication lock.
func CreatePrivatePinnedFileAt(parent *os.File, component string) (*os.File, error) {
	return createPrivateEntry(parent, component, false, false)
}

// CreatePrivateMovableFileAt creates a private writable file with rename access.
func CreatePrivateMovableFileAt(parent *os.File, component string) (*os.File, error) {
	return createPrivateEntry(parent, component, false, true)
}

func createPrivateEntry(parent *os.File, component string, directory, movable bool) (*os.File, error) {
	if parent == nil || component == "" || component == "." || component == ".." ||
		filepath.VolumeName(component) != "" || strings.ContainsAny(component, "/\\:\x00") {
		return nil, errors.New("private entry component is invalid")
	}
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;GA;;;" + user.String() + ")")
	if err != nil {
		return nil, fmt.Errorf("building private entry security: %w", err)
	}
	name, err := windows.NewNTUnicodeString(component)
	if err != nil {
		return nil, fmt.Errorf("encoding private entry component: %w", err)
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()), ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE, SecurityDescriptor: descriptor,
	}
	access := uint32(windows.READ_CONTROL | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT)
	sharing := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if directory {
		access |= windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		access |= windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	if movable {
		access |= windows.DELETE
		sharing |= windows.FILE_SHARE_DELETE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access, attributes, &status, nil, 0, sharing, windows.FILE_CREATE, options, 0, 0)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		return nil, os.ErrExist
	}
	if err != nil {
		return nil, fmt.Errorf("creating private entry: %w", err)
	}
	return os.NewFile(uintptr(handle), "private entry"), nil
}

// ValidatePrivateDirectoryFile checks the opened directory itself without
// changing permissions. Unlike regular-file validators, it requires directory
// type and checks the protected-DACL bit as well as trusted allow entries.
func ValidatePrivateDirectoryFile(file *os.File) error {
	if file == nil {
		return errors.New("private directory handle is invalid")
	}
	handle := windows.Handle(file.Fd())
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return fmt.Errorf("reading private directory type: %w", err)
	}
	if kind != windows.FILE_TYPE_DISK {
		return errors.New("private directory type is invalid")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("reading private directory attributes: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("private directory type is invalid")
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading private directory security: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("reading private directory owner: %w", err)
	}
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	tokenOwner, err := currentTokenOwnerSID()
	if err != nil {
		return err
	}
	if !sidIn(owner, []*windows.SID{user, tokenOwner}) {
		return errors.New("private directory owner is invalid")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("reading private directory DACL protection: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("private directory DACL is not protected")
	}
	return validateRestrictedDACL("private directory", handle)
}
