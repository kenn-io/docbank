//go:build unix

package packstore

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func snapshotPathIdentity(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func openNoFollow(path string, durable bool) (*os.File, error) {
	flags := os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if durable {
		flags = os.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := validateRegularNoFollow(path, info); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func openLooseRepairPin(path string) (*os.File, fs.FileInfo, error) {
	return openLooseFile(path)
}

func validatePlatformFileInfo(fs.FileInfo) error { return nil }
