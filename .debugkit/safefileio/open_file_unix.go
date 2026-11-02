//go:build unix

package safefileio

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// OpenCurrentUserFile opens path without following symlinks and verifies the
// opened handle is a regular file owned by the current user.
func OpenCurrentUserFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	file, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	if err := ValidateCurrentUserFile(file); err != nil {
		return nil, err
	}
	success = true
	return file, nil
}

// ValidateCurrentUserFile verifies an already-open handle is a regular file
// owned by the current user.
func ValidateCurrentUserFile(file *os.File) error {
	if file == nil {
		return errors.New("file is nil")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", file.Name())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", file.Name())
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", file.Name())
	}
	return nil
}
