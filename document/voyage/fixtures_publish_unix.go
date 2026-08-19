//go:build linux || darwin

package voyage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openVerifiedFixtureParent(path string, identity os.FileInfo) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open fixture parent without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open fixture parent descriptor")
	}
	openedIdentity, err := file.Stat()
	if err != nil || !os.SameFile(identity, openedIdentity) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("fixture parent changed before no-replace publication"))
	}
	return file, nil
}

func verifyFixtureStagingAt(parent *os.File, name string, identity os.FileInfo) error {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open fixture staging directory without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open fixture staging directory descriptor")
	}
	defer func() { _ = file.Close() }()
	openedIdentity, err := file.Stat()
	if err != nil || !os.SameFile(identity, openedIdentity) {
		return errors.Join(err, errors.New("fixture staging directory changed before no-replace publication"))
	}
	return nil
}

func openFixtureFileNoFollow(
	rootPath, name string,
	rootIdentity, fileIdentity os.FileInfo,
) (*os.File, error) {
	parent, err := openVerifiedFixtureParent(rootPath, rootIdentity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	fd, err := unix.Openat(
		int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open fixture relative to pinned root: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open fixture descriptor")
	}
	openedIdentity, err := file.Stat()
	if err != nil || !openedIdentity.Mode().IsRegular() || !os.SameFile(fileIdentity, openedIdentity) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("fixture changed during descriptor-relative open"))
	}
	return file, nil
}
