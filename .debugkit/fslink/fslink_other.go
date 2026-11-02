//go:build !unix && !windows

package fslink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
)

// errPlatform fails closed where this package cannot open an entry without
// following links.
var errPlatform = fmt.Errorf("fslink: unsupported on %s: %w", runtime.GOOS, errors.ErrUnsupported)

// classify relies on os.Lstat, which reports symlinks wherever the platform
// has them and has no junctions to miss.
func classify(path string) (Kind, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return NotLink, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return Symlink, nil
	}
	return NotLink, nil
}

func createJunction(target, link string) error {
	return &os.LinkError{Op: "junction", Old: target, New: link, Err: errPlatform}
}

func linkDir(target, link string) (Kind, error) {
	return NotLink, &os.LinkError{Op: "symlink", Old: target, New: link, Err: errPlatform}
}

func openFile(path string, _ int, _ fs.FileMode) (*os.File, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: errPlatform}
}

func openDirPath(path string) (*os.File, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: errPlatform}
}

func openRegular(path string) (*os.File, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: errPlatform}
}

type entry struct{}

func lstatAt(_ *os.Root, name string) (entry, error) {
	return entry{}, &fs.PathError{Op: "lstat", Path: name, Err: errPlatform}
}

func (entry) isDir() bool { return false }

func (entry) sameFile(*os.File) (bool, error) { return false, errPlatform }

func platformSupport() error { return errPlatform }
