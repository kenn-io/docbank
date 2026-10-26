//go:build unix && !linux

package ingest

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

type watchMount uint64

func watchMetadataForFile(_ *os.File, info fs.FileInfo) (watchMount, watchFileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, watchFileIdentity{}, errors.New("file has no Unix filesystem identity")
	}
	device := int64(stat.Dev)
	if device < 0 {
		return 0, watchFileIdentity{}, errors.New("file has an invalid filesystem device")
	}
	mount := watchMount(device)
	identity := watchFileIdentity{object: watchObjectKey{mount: mount, file: stat.Ino}}
	identity.birth = unixWatchBirth(stat)
	return mount, identity, nil
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
	return stat.Ctimespec.Nano()
}
