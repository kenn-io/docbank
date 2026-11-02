package safefileio

import (
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kit/fsname"
	"golang.org/x/sys/unix"
)

// ValidatePrivateCurrentUserFile verifies that an open current-user-owned file
// has private mode bits and no access ACL that could grant another principal.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	return validatePrivateCurrentUserFile(file, validateLinuxPrivateAccess)
}

func validateLinuxPrivateAccess(file *os.File) error {
	remote, err := fsname.RemoteFile(file)
	if err != nil {
		return fmt.Errorf("inspect file filesystem: %w", err)
	}
	var status unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &status); err != nil {
		return fmt.Errorf("inspect file filesystem: %w", err)
	}
	if linuxFilesystemHasExternalAccessPolicy(remote, int64(status.Type)) { //nolint:unconvert // Statfs_t.Type is int32 on 32-bit Linux targets.
		return errors.New(
			"safefileio: private current-user file validation is unsupported " +
				"on filesystems with external access policy",
		)
	}
	return validateLinuxAccessACLs(file)
}

// linuxFilesystemHasExternalAccessPolicy reports whether local mode bits and
// access ACLs cannot establish who may open the file: network and FUSE file
// systems (remote, as fsname.RemoteFile reports them), and AppArmor's
// securityfs, which is local but applies its own access policy.
func linuxFilesystemHasExternalAccessPolicy(remote bool, filesystemType int64) bool {
	return remote || uint32(filesystemType) == uint32(unix.AAFS_MAGIC)
}

func validateLinuxAccessACLs(file *os.File) error {
	for _, attribute := range []string{
		"system.posix_acl_access",
		"system.nfs4_acl",
		"system.cifs_acl",
	} {
		_, err := unix.Fgetxattr(int(file.Fd()), attribute, nil)
		if err == nil {
			return fmt.Errorf("safefileio: file has access ACL %s", attribute)
		}
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
			continue
		}
		return fmt.Errorf("inspect access ACL %s: %w", attribute, err)
	}
	return nil
}
