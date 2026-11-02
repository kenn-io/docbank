//go:build !darwin && !linux && !windows

package atomicfile

import (
	"errors"
	"os"
)

// RenameNoReplace atomically renames oldpath to newpath, failing with an
// error wrapping fs.ErrExist when newpath already exists. This platform has
// no atomic no-replace rename, so it fails with an error wrapping
// errors.ErrUnsupported rather than checking and renaming separately.
func RenameNoReplace(oldpath, newpath string) error {
	return &os.LinkError{Op: "rename-noreplace", Old: oldpath, New: newpath, Err: errors.ErrUnsupported}
}
