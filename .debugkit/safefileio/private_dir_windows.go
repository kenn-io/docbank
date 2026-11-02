//go:build windows

package safefileio

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

// EnsurePrivateDir creates path when needed and verifies it is a non-reparse
// directory owned by the current token user, token owner, LocalSystem, or
// built-in Administrators with a DACL restricted to those trusted principals.
// It rewrites the DACL on every call to repair inherited child permissions.
func EnsurePrivateDir(path string) error {
	return ensurePrivateDir(path, false)
}

// EnsurePrivateDirShallow creates and secures path like EnsurePrivateDir, but
// skips rewriting a protected DACL that exactly matches the private directory ACL.
// It doesn't repair children moved in with other permissions. Use it only for
// directories whose children carry no data.
func EnsurePrivateDirShallow(path string) error {
	return ensurePrivateDir(path, true)
}

func ensurePrivateDir(path string, keepMatching bool) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openWindowsDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return err
	}
	if err := verifyWindowsDirHandle(path, handle, userSID, ownerSID); err != nil {
		return err
	}
	acl, err := privateWindowsACL(userSID, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	if err != nil {
		return err
	}
	if keepMatching && windowsDirDACLMatches(handle, acl) {
		return nil
	}
	return restrictWindowsDir(handle, acl)
}

func windowsDirDACLMatches(handle windows.Handle, acl *windows.ACL) bool {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false
	}
	return control&windows.SE_DACL_PROTECTED != 0 && dacl != nil && bytes.Equal(windowsACLBytes(dacl), windowsACLBytes(acl))
}

// ValidatePrivateDir verifies path is a non-reparse directory owned by the
// current token user, token owner, LocalSystem, or built-in Administrators with
// a DACL restricted to those trusted principals. It never creates or changes
// the directory.
func ValidatePrivateDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openWindowsDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return err
	}
	if err := verifyWindowsDirHandle(path, handle, userSID, ownerSID); err != nil {
		return err
	}
	return verifyWindowsDirDACL(path, handle, userSID, ownerSID)
}

// CurrentUserID returns a stable filesystem-safe identifier for the current
// Windows account.
func CurrentUserID() (string, error) {
	sid, err := currentWindowsUserSID()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sid.String()))
	return "sid-" + hex.EncodeToString(sum[:8]), nil
}

func rejectWindowsReparsePoint(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	path16, err := winpath.UTF16Ptr(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(path16)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	return nil
}

func openWindowsDir(path string) (windows.Handle, error) {
	path16, err := winpath.UTF16Ptr(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		path16,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func verifyWindowsDirHandle(path string, handle windows.Handle, userSID, ownerSID *windows.SID) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s is not a directory", path)
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
	if owner == nil {
		return fmt.Errorf("%s owner is missing", path)
	}
	return verifyWindowsDirectoryOwner(path, owner, userSID, ownerSID)
}

func trustedWindowsDirectoryOwners(userSID, ownerSID *windows.SID) ([]*windows.SID, error) {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	return []*windows.SID{userSID, ownerSID, systemSID, adminsSID}, nil
}

func verifyWindowsDirectoryOwner(path string, owner, userSID, ownerSID *windows.SID) error {
	trusted, err := trustedWindowsDirectoryOwners(userSID, ownerSID)
	if err != nil {
		return err
	}
	if !windowsAnyOwnerMatches(owner, trusted) {
		return fmt.Errorf("%s is not owned by current user, token owner, LocalSystem, or built-in Administrators", path)
	}
	return nil
}

func verifyWindowsDirDACL(path string, handle windows.Handle, userSID, ownerSID *windows.SID) error {
	return verifyWindowsDACL(path, handle, userSID, ownerSID)
}

func verifyWindowsFileDACL(path string, handle windows.Handle, userSID, ownerSID *windows.SID) error {
	return verifyWindowsDACL(path, handle, userSID, ownerSID)
}

func verifyWindowsDACL(
	path string,
	handle windows.Handle,
	userSID, ownerSID *windows.SID,
) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%s DACL is not protected", path)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("%s DACL is empty", path)
	}
	allowed, err := trustedWindowsDirectoryOwners(userSID, ownerSID)
	if err != nil {
		return err
	}
	for i := range dacl.AceCount {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return err
		}
		if ace == nil {
			return fmt.Errorf("%s DACL ACE is missing", path)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%s DACL contains non-allow ACE", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !windowsAnyOwnerMatches(sid, allowed) {
			return fmt.Errorf("%s DACL grants access to unexpected principal", path)
		}
	}
	return nil
}

func windowsAnyOwnerMatches(owner *windows.SID, allowed []*windows.SID) bool {
	for _, sid := range allowed {
		if sid != nil && owner != nil && owner.Equals(sid) {
			return true
		}
	}
	return false
}

func windowsACLBytes(acl *windows.ACL) []byte {
	header := unsafe.Slice((*byte)(unsafe.Pointer(acl)), unsafe.Sizeof(*acl))
	// AclSize is the uint16 at offset 2 in the Win32 ACL header.
	size := binary.LittleEndian.Uint16(header[2:4])
	return unsafe.Slice((*byte)(unsafe.Pointer(acl)), size)
}

func restrictWindowsDir(handle windows.Handle, acl *windows.ACL) error {
	return windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

// privateWindowsACL grants full control to the current user, LocalSystem, and
// built-in Administrators with the given ACE inheritance flags.
func privateWindowsACL(userSID *windows.SID, inheritance uint32) (*windows.ACL, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	entries := []windows.EXPLICIT_ACCESS{
		allowFullControl(userSID, windows.TRUSTEE_IS_USER, inheritance),
	}
	if !userSID.Equals(system) {
		entries = append(entries, allowFullControl(system, windows.TRUSTEE_IS_USER, inheritance))
	}
	if !userSID.Equals(admins) {
		entries = append(entries, allowFullControl(admins, windows.TRUSTEE_IS_GROUP, inheritance))
	}
	return windows.ACLFromEntries(entries, nil)
}

func allowFullControl(
	sid *windows.SID,
	trusteeType windows.TRUSTEE_TYPE,
	inheritance uint32,
) windows.EXPLICIT_ACCESS {
	// FILE_ALL_ACCESS (0x1F01FF) avoids Windows splitting generic inheritable ACEs.
	const fileAllAccess = 0x1F01FF
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
