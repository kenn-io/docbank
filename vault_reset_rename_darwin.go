//go:build darwin

package docbank

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameVaultNoReplace(source, destination string) error {
	if err := unix.RenamexNp(source, destination, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np", Old: source, New: destination, Err: err}
	}
	return nil
}
