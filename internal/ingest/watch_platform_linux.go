//go:build linux

package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

type watchMount uint64

func watchMountForFile(file *os.File) (watchMount, error) {
	var stat unix.Statx_t
	err := unix.Statx(int(file.Fd()), "",
		unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC, unix.STATX_MNT_ID, &stat)
	if err != nil {
		return 0, fmt.Errorf("reading reliable mount identity: %w", err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("kernel did not provide a reliable mount identity")
	}
	return watchMount(stat.Mnt_id), nil
}

func (mount watchMount) same(other watchMount) bool { return mount == other }

func transientWatchObservationError(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
