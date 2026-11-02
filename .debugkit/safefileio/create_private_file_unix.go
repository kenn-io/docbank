//go:build unix

package safefileio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// CreatePrivateFile creates a new regular file at path, opened read-write,
// that only the current user can access from the moment it exists. It fails
// with an error wrapping fs.ErrExist when anything, including a dangling
// symlink, already exists at path. It never repairs an existing file.
func CreatePrivateFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	// Where private access cannot be verified every create would fail
	// validation, so refuse before leaving a file behind.
	if err := privateFileCreationSupported(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(
		path,
		os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	if err := securePrivateFile(file); err != nil {
		return nil, errors.Join(err, discardCreatedFile(path, file))
	}
	return file, nil
}

func securePrivateFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	// ValidatePrivateCurrentUserFile also checks type and ownership, and fails
	// closed on Unix platforms where private access cannot be verified.
	return ValidatePrivateCurrentUserFile(file)
}

// discardCreatedFile closes file and removes the entry at path only while it
// still names the file this package created, so a replaced entry is never
// deleted. Unix cannot unlink by handle, so it pins the parent directory as
// an os.Root and does the identity check and the removal through that one
// directory handle, which renaming an ancestor cannot redirect. Removal also
// requires a parent that no other user can rename entries in; otherwise the
// empty private file is left in place.
func discardCreatedFile(path string, file *os.File) error {
	created, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return errors.Join(fmt.Errorf("stat created file %s: %w", path, statErr), closeErr)
	}
	leave := func(err error) error {
		return errors.Join(fmt.Errorf("leaving created file %s in place: %w", path, err), closeErr)
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return leave(err)
	}
	defer func() { _ = parent.Close() }()
	if err := requireUnsharedRoot(parent, created); err != nil {
		return leave(err)
	}
	base := filepath.Base(path)
	current, err := parent.Lstat(base)
	if err != nil {
		return errors.Join(fmt.Errorf("lstat created file %s: %w", path, err), closeErr)
	}
	if !os.SameFile(created, current) {
		return errors.Join(fmt.Errorf("%s was replaced after creation; not removing it", path), closeErr)
	}
	if err := parent.Remove(base); err != nil {
		return errors.Join(fmt.Errorf("remove created file %s: %w", path, err), closeErr)
	}
	return closeErr
}

// requireUnsharedRoot reports an error unless only the current user can
// rename entries in the pinned directory: its mode and owner must pass
// requireUnsharedParent, and its file system and ACLs must not grant access
// that mode bits do not show.
func requireUnsharedRoot(parent *os.Root, created fs.FileInfo) error {
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if err := requireUnsharedParent(info, created); err != nil {
		return err
	}
	return verifyParentAccessPolicy(dir)
}

// requireUnsharedParent reports an error unless only the current user or root
// can rename entries in the directory described by dir: it must be owned by
// one of them and either deny group and other write access, or have the
// sticky bit set while created belongs to the current user or root, since the
// sticky bit only stops others from renaming entries they do not own.
func requireUnsharedParent(dir, created fs.FileInfo) error {
	if !dir.IsDir() {
		return fmt.Errorf("parent %s is not a directory", dir.Name())
	}
	stat, ok := dir.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", dir.Name())
	}
	uid := uint32(os.Getuid())
	if stat.Uid != uid && stat.Uid != 0 {
		return fmt.Errorf("parent %s is owned by another user", dir.Name())
	}
	if dir.Mode().Perm()&0o022 == 0 {
		return nil
	}
	if dir.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("other users can rename entries in parent %s", dir.Name())
	}
	createdStat, ok := created.Sys().(*syscall.Stat_t)
	if !ok || (createdStat.Uid != uid && createdStat.Uid != 0) {
		return fmt.Errorf("parent %s is shared and the created file has an untrusted owner", dir.Name())
	}
	return nil
}
