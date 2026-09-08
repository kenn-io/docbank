package qmdexport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type entryRole uint8

const (
	stableEntry entryRole = iota
	movableEntry
)

var (
	errDirectoryBound = errors.New("qmd export directory exceeds bound")
	errEntry          = errors.New("qmd export entry is invalid")
	errPrivate        = errors.New("qmd export entry is not private")
	errIdentity       = errors.New("qmd export entry identity changed")
	errLockBusy       = errors.New("qmd export lock is busy")
)

type entryIdentity struct{ info os.FileInfo }

// Preserve the native cause for errors.Is without putting filesystem-derived
// names, paths or identity values in diagnostic text.
type nativeOperationError struct{ cause error }

func (e nativeOperationError) Error() string { return "qmd export native operation failed" }
func (e nativeOperationError) Unwrap() error { return e.cause }
func nativeError(err error) error {
	if err == nil {
		return nil
	}
	return nativeOperationError{cause: err}
}

// anchoredDir owns only file. Operations borrow their parent; no method uses
// the file's descriptive name as pathname authority.
type anchoredDir struct{ file *os.File }

func validComponent(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.VolumeName(name) == "" &&
		!strings.ContainsAny(name, "/\\:\x00")
}

func (d *anchoredDir) Close() error {
	if d == nil || d.file == nil {
		return nil
	}
	f := d.file
	d.file = nil
	return f.Close()
}

func (d *anchoredDir) openDir(name string) (*anchoredDir, entryIdentity, error) {
	return d.openDirectory(name, true, true)
}

// openMovableDir permits verification while a creation handle with rename
// access remains open. Stable acquisition must use openDir instead.
func (d *anchoredDir) openMovableDir(name string) (*anchoredDir, entryIdentity, error) {
	return d.openDirectory(name, false, true)
}

func (d *anchoredDir) openDirectory(name string, pinned, private bool) (*anchoredDir, entryIdentity, error) {
	if !validComponent(name) {
		return nil, entryIdentity{}, errEntry
	}
	f, err := nativeOpen(d, name, true, pinned)
	if err != nil {
		return nil, entryIdentity{}, err
	}
	info, err := f.Stat()
	if err == nil && !info.IsDir() {
		err = errEntry
	}
	if err == nil && private {
		err = validatePrivateEntry(f, true)
	}
	if err != nil {
		return nil, entryIdentity{}, errors.Join(err, f.Close())
	}
	return &anchoredDir{file: f}, entryIdentity{info: info}, nil
}

func (d *anchoredDir) createDir(name string, role entryRole) (*anchoredDir, entryIdentity, error) {
	f, id, err := d.create(name, role, true)
	if err != nil {
		return nil, entryIdentity{}, err
	}
	return &anchoredDir{file: f}, id, nil
}

func (d *anchoredDir) createFile(name string, role entryRole) (*os.File, entryIdentity, error) {
	return d.create(name, role, false)
}

func (d *anchoredDir) create(name string, role entryRole, directory bool) (*os.File, entryIdentity, error) {
	if !validComponent(name) || (role != stableEntry && role != movableEntry) {
		return nil, entryIdentity{}, errEntry
	}
	f, err := nativeCreate(d, name, directory, role)
	if err != nil {
		return nil, entryIdentity{}, err
	}
	info, err := f.Stat()
	if err == nil {
		err = validatePrivateEntry(f, directory)
	}
	if err != nil {
		return nil, entryIdentity{}, errors.Join(err, f.Close())
	}
	return f, entryIdentity{info: info}, nil
}

func (d *anchoredDir) openFile(name string) (*os.File, entryIdentity, error) {
	return d.openRegular(name, false)
}

func (d *anchoredDir) openRegular(name string, pinned bool) (*os.File, entryIdentity, error) {
	if !validComponent(name) {
		return nil, entryIdentity{}, errEntry
	}
	f, err := nativeOpen(d, name, false, pinned)
	if err != nil {
		return nil, entryIdentity{}, err
	}
	info, err := f.Stat()
	if err == nil {
		err = validatePrivateEntry(f, false)
	}
	if err != nil {
		return nil, entryIdentity{}, errors.Join(err, f.Close())
	}
	return f, entryIdentity{info: info}, nil
}

func (d *anchoredDir) sameEntry(name string, identity entryIdentity) error {
	if !validComponent(name) || identity.info == nil {
		return errEntry
	}
	f, err := nativeOpen(d, name, identity.info.IsDir(), false)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && !os.SameFile(info, identity.info) {
		err = errIdentity
	}
	return errors.Join(err, f.Close())
}

func (d *anchoredDir) removeEntry(name string, identity entryIdentity, directory bool) error {
	if !validComponent(name) || identity.info == nil || identity.info.IsDir() != directory {
		return errEntry
	}
	return nativeRemove(d, name, identity, directory)
}

func (d *anchoredDir) entries(ctx context.Context, maximum int) (result []os.DirEntry, err error) {
	if ctx == nil {
		return nil, errEntry
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maximum < 0 {
		return nil, errDirectoryBound
	}
	f, err := nativeReopenDirectory(d)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	held, err := d.file.Stat()
	if err != nil {
		return nil, err
	}
	reopened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(held, reopened) {
		return nil, errIdentity
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := f.ReadDir(min(64, maximum-len(result)+1))
		if len(batch) > maximum-len(result) {
			return nil, errDirectoryBound
		}
		result = append(result, batch...)
		if errors.Is(err, io.EOF) {
			return result, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
	}
}

func heldIdentity(file *os.File, identity entryIdentity) error {
	if file == nil || identity.info == nil {
		return errEntry
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, identity.info) {
		return errIdentity
	}
	return nil
}

func moveDirectoryNoReplace(sourceParent *anchoredDir, name string, source *anchoredDir, identity entryIdentity, targetParent *anchoredDir, target string) error {
	if !validComponent(name) || !validComponent(target) || source == nil || identity.info == nil || !identity.info.IsDir() {
		return errEntry
	}
	if err := heldIdentity(source.file, identity); err != nil {
		return err
	}
	if err := sourceParent.sameEntry(name, identity); err != nil {
		return err
	}
	return nativeMove(sourceParent, name, source.file, targetParent, target, false)
}

func replaceFile(sourceParent *anchoredDir, name string, source *os.File, identity entryIdentity, targetParent *anchoredDir, target string) error {
	if !validComponent(name) || !validComponent(target) || identity.info == nil || !identity.info.Mode().IsRegular() {
		return errEntry
	}
	if err := heldIdentity(source, identity); err != nil {
		return err
	}
	if err := sourceParent.sameEntry(name, identity); err != nil {
		return err
	}
	// Existing destinations must be regular private files. The native rename
	// remains atomic; this check does not supply hostile same-user isolation.
	destination, _, err := targetParent.openFile(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if destination != nil {
		if err := destination.Close(); err != nil {
			return err
		}
	}
	return nativeMove(sourceParent, name, source, targetParent, target, true)
}

func confirmSelectedFile(file *os.File) error { return file.Sync() }
