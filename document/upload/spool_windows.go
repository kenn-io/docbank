//go:build windows

package upload

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/winsecurity"
)

type spoolDirectory struct {
	base     string
	name     string
	baseRoot *os.Root
	root     *os.Root
	pin      *os.File
}

func openSpoolDirectory(base string) (*spoolDirectory, error) {
	name, err := randomSpoolName()
	if err != nil {
		return nil, err
	}
	baseRoot, err := openStableRoot(base)
	if err != nil {
		return nil, err
	}
	pin, err := winsecurity.MkdirPrivatePinnedAt(baseRoot, name)
	if err != nil {
		_ = baseRoot.Close()
		return nil, err
	}
	root, err := baseRoot.OpenRoot(name)
	if err != nil {
		_ = pin.Close()
		_ = baseRoot.Remove(name)
		_ = baseRoot.Close()
		return nil, err
	}
	return &spoolDirectory{base: base, name: name, baseRoot: baseRoot, root: root, pin: pin}, nil
}

func (directory *spoolDirectory) create(name string) (*os.File, error) {
	return directory.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func (directory *spoolDirectory) openReader(name string, written os.FileInfo) (*os.File, error) {
	linkInfo, err := directory.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
		return nil, errors.New("spool reader path is a reparse point or non-regular file")
	}
	reader, err := directory.root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := reader.Stat()
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	if !os.SameFile(written, info) || info.Size() != written.Size() {
		_ = reader.Close()
		return nil, errors.New("spool reader does not name the synced writer identity")
	}
	return reader, nil
}

func (directory *spoolDirectory) unlink(name string) error {
	return directory.root.Remove(name)
}

func (directory *spoolDirectory) sync() error { return nil }

func (directory *spoolDirectory) path(name string) string {
	return filepath.Join(directory.base, directory.name, name)
}

func (directory *spoolDirectory) cleanup() error {
	if directory == nil {
		return nil
	}
	var result error
	if directory.root != nil {
		entries, err := fs.ReadDir(directory.root.FS(), ".")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			result = err
		}
		for _, entry := range entries {
			if err := directory.root.RemoveAll(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		result = errors.Join(result, directory.root.Close())
		directory.root = nil
	}
	if directory.pin != nil {
		result = errors.Join(result, directory.pin.Close())
		directory.pin = nil
	}
	if directory.baseRoot != nil {
		if err := directory.baseRoot.Remove(directory.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove private spool directory: %w", err))
		}
		result = errors.Join(result, directory.baseRoot.Close())
		directory.baseRoot = nil
	}
	return result
}
