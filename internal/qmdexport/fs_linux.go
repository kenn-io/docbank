package qmdexport

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func nativeMove(sourceParent *anchoredDir, name string, source *os.File, targetParent *anchoredDir, target string, replace bool) error {
	if replace {
		return nativeError(unix.Renameat(int(sourceParent.file.Fd()), name, int(targetParent.file.Fd()), target))
	}
	return nativeError(unix.Renameat2(int(sourceParent.file.Fd()), name, int(targetParent.file.Fd()), target, unix.RENAME_NOREPLACE))
}

// Match Kit's conservative native access policy on the held directory too:
// ACL-bearing or externally governed filesystems cannot establish privacy.
func nativeDirectoryACL(file *os.File) error {
	var status unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &status); err != nil {
		return nativeError(err)
	}
	switch status.Type {
	case unix.AAFS_MAGIC, unix.AFS_FS_MAGIC, unix.AFS_SUPER_MAGIC,
		unix.CEPH_SUPER_MAGIC, unix.CIFS_SUPER_MAGIC, unix.CODA_SUPER_MAGIC,
		unix.FUSE_SUPER_MAGIC, unix.NCP_SUPER_MAGIC, unix.NFS_SUPER_MAGIC,
		unix.SMB_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.V9FS_MAGIC:
		return errPrivate
	}
	for _, attribute := range []string{"system.posix_acl_access", "system.nfs4_acl", "system.cifs_acl"} {
		_, err := unix.Fgetxattr(int(file.Fd()), attribute, nil)
		if err == nil {
			return errPrivate
		}
		if !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
			return errPrivate
		}
	}
	return nil
}
