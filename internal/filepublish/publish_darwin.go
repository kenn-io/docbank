//go:build darwin

package filepublish

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(stagedPath, destinationPath string) error {
	if err := unix.RenamexNp(stagedPath, destinationPath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np", Old: stagedPath, New: destinationPath, Err: err}
	}
	return nil
}
