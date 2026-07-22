//go:build darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameGetFileNoReplace(stagedPath, outputPath string) error {
	if err := unix.RenamexNp(stagedPath, outputPath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np", Old: stagedPath, New: outputPath, Err: err}
	}
	return nil
}
