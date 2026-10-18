//go:build linux

package filepublish

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(stagedPath, destinationPath string) error {
	return renameAtNoReplace(unix.AT_FDCWD, stagedPath, unix.AT_FDCWD, destinationPath)
}

func renameAtNoReplace(source int, stagedPath string, destination int, destinationPath string) error {
	if err := unix.Renameat2(source, stagedPath, destination, destinationPath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "renameat2", Old: stagedPath, New: destinationPath, Err: err}
	}
	return nil
}
