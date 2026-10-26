//go:build windows

package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type watchMount uint32

func watchMetadataForFile(file *os.File, _ fs.FileInfo) (watchMount, watchFileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, watchFileIdentity{}, fmt.Errorf("reading %s file information: %w", file.Name(), err)
	}
	mount := watchMount(info.VolumeSerialNumber)
	ticks := uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime)
	identity := watchFileIdentity{
		object: watchObjectKey{mount: mount, file: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)},
		birth:  watchBirthFromFiletimeTicks(ticks),
	}
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
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// fileBasicInfo matches the 40-byte FILE_BASIC_INFO ABI, including trailing
// alignment on both supported Windows architectures.
type fileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	fileAttributes uint32
	padding        uint32
}

func fileChangeTime(file *os.File, _ fs.FileInfo) int64 {
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileBasicInfo,
		// #nosec G103 -- basic is a 40-byte, 8-byte-aligned FILE_BASIC_INFO buffer, live throughout this synchronous call.
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return 0
	}
	if basic.changeTime <= 0 {
		return 0
	}
	return basic.changeTime
}
