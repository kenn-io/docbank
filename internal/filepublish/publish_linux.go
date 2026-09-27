//go:build linux

package filepublish

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(stagedPath, destinationPath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, stagedPath, unix.AT_FDCWD, destinationPath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "renameat2", Old: stagedPath, New: destinationPath, Err: err}
	}
	return nil
}

func replaceFile(stagedPath, destinationPath string) error {
	return os.Rename(stagedPath, destinationPath)
}
