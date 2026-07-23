//go:build linux

package docbank

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameVaultNoReplace(source, destination string) error {
	if err := unix.Renameat2(
		unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE,
	); err != nil {
		return &os.LinkError{Op: "renameat2", Old: source, New: destination, Err: err}
	}
	return nil
}
