//go:build darwin

package atomicfile

import (
	"os"

	"golang.org/x/sys/unix"
)

// RenameNoReplace atomically renames oldpath to newpath, failing with an
// error wrapping fs.ErrExist when newpath already exists. On macOS it uses
// renamex_np with RENAME_EXCL.
func RenameNoReplace(oldpath, newpath string) error {
	if err := unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
