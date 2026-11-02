package fsname

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func remote(path string) (bool, error) {
	var status unix.Statfs_t
	if err := unix.Statfs(path, &status); err != nil {
		return false, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return linuxRemoteType(int64(status.Type)), nil //nolint:unconvert // Statfs_t.Type is int32 on 32-bit Linux targets.
}

// RemoteFile reports whether the open file f lives on a network or
// user-space file system. See Remote.
func RemoteFile(f *os.File) (bool, error) {
	var status unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &status); err != nil {
		return false, &fs.PathError{Op: "fstatfs", Path: f.Name(), Err: err}
	}
	return linuxRemoteType(int64(status.Type)), nil //nolint:unconvert // Statfs_t.Type is int32 on 32-bit Linux targets.
}

// linuxRemoteType reports whether a statfs f_type magic names a network or
// FUSE file system. It compares the low 32 bits because Statfs_t.Type is
// int32 on 32-bit targets, where magics above 0x7fffffff arrive negative.
func linuxRemoteType(fsType int64) bool {
	switch uint32(fsType) {
	case uint32(unix.AFS_FS_MAGIC),
		uint32(unix.AFS_SUPER_MAGIC),
		uint32(unix.CEPH_SUPER_MAGIC),
		uint32(unix.CIFS_SUPER_MAGIC),
		uint32(unix.CODA_SUPER_MAGIC),
		uint32(unix.FUSE_SUPER_MAGIC),
		uint32(unix.NCP_SUPER_MAGIC),
		uint32(unix.NFS_SUPER_MAGIC),
		uint32(unix.SMB_SUPER_MAGIC),
		uint32(unix.SMB2_SUPER_MAGIC),
		uint32(unix.V9FS_MAGIC):
		return true
	default:
		return false
	}
}
