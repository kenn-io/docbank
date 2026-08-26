//go:build !windows

package upload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type spoolDirectory struct {
	base string
	name string
	root *os.File
	dir  *os.File
}

func openSpoolDirectory(base string) (*spoolDirectory, error) {
	name, err := randomSpoolName()
	if err != nil {
		return nil, fmt.Errorf("create no-follow spool: %w", err)
	}
	rootFD, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open no-follow spool root: %w", err)
	}
	root := os.NewFile(uintptr(rootFD), base)
	if err := unix.Mkdirat(rootFD, name, 0o700); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("create private spool directory: %w", err)
	}
	dirFD, err := unix.Openat(rootFD, name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Unlinkat(rootFD, name, unix.AT_REMOVEDIR)
		_ = root.Close()
		return nil, fmt.Errorf("open private spool directory: %w", err)
	}
	dir := os.NewFile(uintptr(dirFD), name)
	return &spoolDirectory{base: base, name: name, root: root, dir: dir}, nil
}

func (directory *spoolDirectory) create(name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.dir.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create no-follow spool: %w", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (directory *spoolDirectory) openReader(name string, written os.FileInfo) (*os.File, error) {
	fd, err := unix.Openat(int(directory.dir.Fd()), name,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open no-follow spool: %w", err)
	}
	reader := os.NewFile(uintptr(fd), name)
	info, err := reader.Stat()
	if err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("stat no-follow spool reader: %w", err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(written, info) || info.Size() != written.Size() {
		_ = reader.Close()
		return nil, errors.New("spool reader does not name the synced writer identity")
	}
	return reader, nil
}

func (directory *spoolDirectory) unlink(name string) error {
	if err := unix.Unlinkat(int(directory.dir.Fd()), name, 0); err != nil {
		return fmt.Errorf("unlink descriptor-relative spool: %w", err)
	}
	return nil
}

func (directory *spoolDirectory) sync() error {
	return errors.Join(directory.dir.Sync(), directory.root.Sync())
}

func (directory *spoolDirectory) path(name string) string {
	return filepath.Join(directory.base, directory.name, name)
}

func (directory *spoolDirectory) cleanup() error {
	if directory == nil {
		return nil
	}
	if directory.dir != nil {
		_ = unix.Unlinkat(int(directory.dir.Fd()), spoolFilename, 0)
		_, _ = directory.dir.Seek(0, 0)
		if names, err := directory.dir.Readdirnames(-1); err == nil {
			for _, name := range names {
				_ = unix.Unlinkat(int(directory.dir.Fd()), name, 0)
			}
		}
	}
	var result error
	if directory.dir != nil {
		result = directory.dir.Close()
		directory.dir = nil
	}
	if directory.root != nil {
		if err := unix.Unlinkat(int(directory.root.Fd()), directory.name, unix.AT_REMOVEDIR); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove private spool directory: %w", err))
		}
		result = errors.Join(result, directory.root.Sync(), directory.root.Close())
		directory.root = nil
	}
	return result
}
