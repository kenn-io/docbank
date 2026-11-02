package fsname

import "golang.org/x/sys/unix"

func statfsFields(status *unix.Statfs_t) (flags uint64, typeName []byte) {
	return uint64(status.F_flags), status.F_fstypename[:]
}
