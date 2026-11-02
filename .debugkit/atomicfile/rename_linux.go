//go:build linux

package atomicfile

import (
	"os"

	"golang.org/x/sys/unix"
)

// RenameNoReplace atomically renames oldpath to newpath, failing with an
// error wrapping fs.ErrExist when newpath already exists. On Linux it uses
// renameat2 with RENAME_NOREPLACE; filesystems without that flag fail rather
// than fall back to a racy check-then-rename.
func RenameNoReplace(oldpath, newpath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "renameat2", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
