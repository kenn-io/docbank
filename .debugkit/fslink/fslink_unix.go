//go:build unix

package fslink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

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
	return &os.LinkError{
		Op:  "junction",
		Old: target,
		New: link,
		Err: fmt.Errorf("fslink: junctions exist only on Windows: %w", errors.ErrUnsupported),
	}
}

func linkDir(target, link string) (Kind, error) {
	if err := os.Symlink(target, link); err != nil {
		return NotLink, err
	}
	return Symlink, nil
}

func openFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, perm)
	if err != nil {
		return nil, linkOpenError(path, err)
	}
	return file, nil
}

// openDirPath opens the directory at path without following a link in its
// final component. O_DIRECTORY fails on a FIFO before the open could wait
// for a writer.
func openDirPath(path string) (*os.File, error) {
	file, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOTDIR) {
		// Some kernels check O_DIRECTORY before O_NOFOLLOW; report a link
		// as a link.
		if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, &fs.PathError{Op: "open", Path: path, Err: ErrIsLink}
		}
	}
	if err != nil {
		return nil, linkOpenError(path, err)
	}
	return file, nil
}

func openRegular(path string) (*os.File, error) {
	// O_NONBLOCK keeps a FIFO open from waiting for a writer; the file-type
	// check below rejects it before any read.
	file, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, linkOpenError(path, err)
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &fs.PathError{Op: "open", Path: path, Err: fmt.Errorf("%w: %s", errNotRegular, info.Mode().Type())}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// linkOpenError maps the errno O_NOFOLLOW produces for a final-component
// symlink (see noFollowErrnos) to ErrIsLink.
func linkOpenError(path string, err error) error {
	for _, errno := range noFollowErrnos {
		if errors.Is(err, errno) {
			return &fs.PathError{Op: "open", Path: path, Err: ErrIsLink}
		}
	}
	return err
}

type entry struct {
	info fs.FileInfo
}

func lstatAt(dir *os.Root, name string) (entry, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return entry{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return entry{}, ErrIsLink
	}
	return entry{info: info}, nil
}

func (e entry) isDir() bool {
	return e.info.IsDir()
}

func (e entry) sameFile(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(e.info, info), nil
}

func platformSupport() error { return nil }
