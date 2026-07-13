//go:build windows

// Package winsecurity contains the Windows-only primitives that must operate
// on already-open handles rather than pathnames.
package winsecurity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MkdirPrivateAt creates component relative to parent with a protected DACL
// granting access only to the current user. Supplying the descriptor to the
// create itself avoids a window in which inherited permissions expose a newly
// created vault directory.
func MkdirPrivateAt(parent *os.Root, component string) error {
	parentFile, err := parent.Open(".")
	if err != nil {
		return fmt.Errorf("opening held parent directory: %w", err)
	}
	defer func() { _ = parentFile.Close() }()

	user, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("resolving current Windows user: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"D:P(A;OICI;GA;;;" + user.String() + ")",
	)
	if err != nil {
		return fmt.Errorf("building private directory security descriptor: %w", err)
	}
	name, err := windows.NewNTUnicodeString(component)
	if err != nil {
		return fmt.Errorf("encoding vault directory component: %w", err)
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      windows.Handle(parentFile.Fd()),
		ObjectName:         name,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err == windows.STATUS_OBJECT_NAME_COLLISION {
		return nil
	}
	if err != nil {
		return err
	}
	return windows.CloseHandle(handle)
}

// OpenRestrictedCurrentUserFile opens a regular file without following a
// final reparse point, verifies current-user ownership, and rejects a DACL
// granting any principal outside the current user and trusted system actors.
func OpenRestrictedCurrentUserFile(path string) (*os.File, error) {
	handle, err := openCurrentUserFileHandle(path, windows.GENERIC_READ|windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	if err := validateRestrictedDACL(path, handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

// RestrictCurrentUserFile verifies that path is a current-user-owned regular
// file opened without following reparse points, then replaces its DACL with a
// protected ACL limited to the current user and trusted system actors.
func RestrictCurrentUserFile(path string) error {
	handle, err := openCurrentUserFileHandle(
		path,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC,
	)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	allowed, err := trustedSIDs()
	if err != nil {
		return fmt.Errorf("resolving trusted Windows principals: %w", err)
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(allowed))
	for i, sid := range allowed {
		trusteeType := windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_USER)
		if i == len(allowed)-1 {
			trusteeType = windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_GROUP)
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  trusteeType,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("building private DACL for %s: %w", path, err)
	}
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("securing %s: %w", path, err)
	}
	return nil
}

func openCurrentUserFileHandle(path string, access uint32) (windows.Handle, error) {
	extended, err := ExtendedLengthPath(path)
	if err != nil {
		return 0, err
	}
	path16, err := windows.UTF16PtrFromString(extended)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		path16,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, err
	}
	if err := validateCurrentUserFileHandle(path, handle); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func validateCurrentUserFileHandle(path string, handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("%s is a directory", path)
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
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
		return fmt.Errorf("%s is not owned by the current user or token owner", path)
	}
	return nil
}

func validateRestrictedDACL(path string, handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("reading %s DACL: %w", path, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("decoding %s DACL: %w", path, err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		return fmt.Errorf("%s DACL is empty", path)
	}
	allowed, err := trustedSIDs()
	if err != nil {
		return fmt.Errorf("resolving trusted Windows principals: %w", err)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("reading %s DACL entry: %w", path, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%s DACL contains a non-allow entry", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sidIn(sid, allowed) {
			return fmt.Errorf("%s DACL grants access to an unexpected principal", path)
		}
	}
	return nil
}

func trustedSIDs() ([]*windows.SID, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	return []*windows.SID{user, system, admins}, nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

type tokenOwner struct {
	Owner *windows.SID
}

func currentTokenOwnerSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	size := uint32(64)
	for {
		buffer := make([]byte, size)
		err := windows.GetTokenInformation(
			token,
			windows.TokenOwner,
			&buffer[0],
			uint32(len(buffer)),
			&size,
		)
		if err == nil {
			owner := (*tokenOwner)(unsafe.Pointer(&buffer[0]))
			if owner.Owner == nil {
				return nil, fmt.Errorf("current token owner is missing")
			}
			return owner.Owner.Copy()
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER || size <= uint32(len(buffer)) {
			return nil, err
		}
	}
}

func sidIn(candidate *windows.SID, allowed []*windows.SID) bool {
	for _, sid := range allowed {
		if candidate != nil && sid != nil && candidate.Equals(sid) {
			return true
		}
	}
	return false
}

// ExtendedLengthPath returns an absolute CreateFileW path that is not subject
// to MAX_PATH. UNC paths use the distinct \\?\UNC\ namespace.
func ExtendedLengthPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, `\\?\`) || strings.HasPrefix(abs, `\\.\`) {
		return abs, nil
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`), nil
	}
	return `\\?\` + abs, nil
}
