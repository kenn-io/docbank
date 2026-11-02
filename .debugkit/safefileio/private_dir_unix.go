//go:build unix

package safefileio

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// EnsurePrivateDir creates path when needed and verifies it is a non-symlink
// directory owned by the current user with mode 0700.
func EnsurePrivateDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", path)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", path)
	}
	if info.Mode().Perm() != 0o700 {
		dir, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open private dir: %w", err)
		}
		defer func() { _ = dir.Close() }()
		if err := dir.Chmod(0o700); err != nil {
			return fmt.Errorf("chmod private dir: %w", err)
		}
		info, err = dir.Stat()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		if info.Mode().Perm() != 0o700 {
			return fmt.Errorf("%s is not mode 0700", path)
		}
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", path)
		}
	}
	return nil
}

// EnsurePrivateDirShallow creates and secures path like EnsurePrivateDir.
func EnsurePrivateDirShallow(path string) error {
	return EnsurePrivateDir(path)
}

// ValidatePrivateDir verifies path is a non-symlink directory owned by the
// current user with mode 0700. It never creates or chmods the directory.
func ValidatePrivateDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", path)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s is not mode 0700", path)
	}
	return nil
}
