//go:build linux

package packstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// openLooseRemovalIdentityPin uses O_PATH so namespace cleanup needs search
// permission on the parent, not read permission on the regular file itself.
// O_NOFOLLOW exposes symlinks to the caller's descriptor-mode validation.
func openLooseRemovalIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, nil, errors.Join(fmt.Errorf("packstore: create Linux identity pin for %s", path), unix.Close(fd))
	}
	identity, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return &fileIdentityPin{File: file}, identity, nil
}
