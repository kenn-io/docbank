//go:build linux

package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type watchMount uint64

func watchMetadataForFile(file *os.File, _ fs.FileInfo) (watchMount, watchFileIdentity, error) {
	var stat unix.Statx_t
	err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC,
		unix.STATX_MNT_ID|unix.STATX_INO|unix.STATX_BTIME, &stat)
	if err != nil {
		return 0, watchFileIdentity{}, fmt.Errorf("reading reliable mount identity: %w", err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, watchFileIdentity{}, errors.New("kernel did not provide a reliable mount identity")
	}
	mount := watchMount(stat.Mnt_id)
	return mount, watchIdentityFromStatx(mount, stat), nil
}
func watchIdentityFromStatx(mount watchMount, stat unix.Statx_t) watchFileIdentity {
	identity := watchFileIdentity{object: watchObjectKey{mount: mount}}
	if stat.Mask&unix.STATX_INO != 0 {
		identity.object.file = stat.Ino
	}
	if stat.Mask&(unix.STATX_INO|unix.STATX_BTIME) == unix.STATX_INO|unix.STATX_BTIME {
		identity.birth = validWatchBirth(stat.Btime.Sec, int64(stat.Btime.Nsec))
	}
	return identity
}

func watchMountForFile(file *os.File) (watchMount, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	mount, _, err := watchMetadataForFile(file, info)
	return mount, err
}
func (mount watchMount) same(other watchMount) bool { return mount == other }

func transientWatchObservationError(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func fileChangeTime(_ *os.File, info fs.FileInfo) int64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ctim.Nano()
}
