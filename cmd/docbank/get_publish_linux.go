//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameGetFileNoReplace(stagedPath, outputPath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, stagedPath, unix.AT_FDCWD, outputPath,
		unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "renameat2", Old: stagedPath, New: outputPath, Err: err}
	}
	return nil
}
