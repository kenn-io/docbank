//go:build !linux && !darwin && !dragonfly && !freebsd && !openbsd && !windows

package fsname

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

func remote(path string) (bool, error) {
	return false, fmt.Errorf("fsname: remote file system detection for %q on %s: %w", path, runtime.GOOS, errors.ErrUnsupported)
}

// RemoteFile reports whether the open file f lives on a network or
// user-space file system. See Remote.
func RemoteFile(f *os.File) (bool, error) {
	return false, fmt.Errorf("fsname: remote file system detection for %q on %s: %w", f.Name(), runtime.GOOS, errors.ErrUnsupported)
}
