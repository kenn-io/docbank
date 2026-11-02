//go:build darwin || dragonfly || freebsd || openbsd

package fsname

import (
	"bytes"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func remote(path string) (bool, error) {
	var status unix.Statfs_t
	if err := unix.Statfs(path, &status); err != nil {
		return false, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	flags, typeName := statfsFields(&status)
	return bsdRemote(flags, typeName), nil
}

// RemoteFile reports whether the open file f lives on a network or
// user-space file system. See Remote.
func RemoteFile(f *os.File) (bool, error) {
	var status unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &status); err != nil {
		return false, &fs.PathError{Op: "fstatfs", Path: f.Name(), Err: err}
	}
	flags, typeName := statfsFields(&status)
	return bsdRemote(flags, typeName), nil
}

// bsdRemote reports a file system as remote when the kernel does not mark
// the mount MNT_LOCAL, or when its type is FUSE: FUSE mounts may carry
// MNT_LOCAL while a user-space process still serves every request.
// typeName is the NUL-terminated f_fstypename array.
func bsdRemote(flags uint64, typeName []byte) bool {
	if flags&unix.MNT_LOCAL == 0 {
		return true
	}
	name, _, _ := bytes.Cut(typeName, []byte{0})
	return fuseTypeName(string(name))
}

// fuseTypeName matches the FUSE type names: macfuse and osxfuse on macOS,
// fusefs and fusefs.<subtype> on FreeBSD, fuse on OpenBSD and DragonFly.
func fuseTypeName(name string) bool {
	return name == "macfuse" || name == "osxfuse" || strings.HasPrefix(name, "fuse")
}
