//go:build windows

package ingest

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

type watchMount uint32

func watchMountForFile(file *os.File) (watchMount, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, err
	}
	return watchMount(info.VolumeSerialNumber), nil
}

func (mount watchMount) same(other watchMount) bool { return mount == other }

func transientWatchObservationError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
