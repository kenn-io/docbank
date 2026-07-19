//go:build unix && !linux

package ingest

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

type watchMount uint64

func watchMountForFile(file *os.File) (watchMount, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("file has no Unix filesystem identity")
	}
	device := int64(stat.Dev)
	if device < 0 {
		return 0, errors.New("file has an invalid filesystem device")
	}
	return watchMount(device), nil
}

func (mount watchMount) same(other watchMount) bool { return mount == other }

func transientWatchObservationError(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
