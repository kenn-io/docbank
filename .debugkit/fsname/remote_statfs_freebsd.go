package fsname

import "golang.org/x/sys/unix"

func statfsFields(status *unix.Statfs_t) (flags uint64, typeName []byte) {
	return status.Flags, status.Fstypename[:]
}
