package packstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/pathresolve"
)

var syncFilesystemRootDir = syncRootDir

// openFilesystemRoot resolves only the configured root, then ties the held
// descriptor back to that resolved pathname. Canonical descendants must be
// opened separately without following links.
func openFilesystemRoot(layout Layout) (*os.Root, error) {
	resolved, err := pathresolve.EvalSymlinks(layout.Root())
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	held, heldErr := root.Stat(".")
	current, currentErr := os.Stat(resolved)
	if heldErr != nil || currentErr != nil || !os.SameFile(held, current) {
		return nil, errors.Join(
			errors.New("packstore: filesystem root changed while opening it"),
			heldErr, currentErr, root.Close(),
		)
	}
	return root, nil
}

// openRootDirNoSymlinks walks rel one component at a time and retains the
// final directory handle. A component replaced while it is opened is rejected
// by the second no-follow identity check.
func openRootDirNoSymlinks(root *os.Root, rel string) (*os.Root, error) {
	return rootDirNoSymlinks(root, rel, false, false)
}

func ensureRootDirNoSymlinks(root *os.Root, rel string, durable bool) (*os.Root, error) {
	return rootDirNoSymlinks(root, rel, true, durable)
}

func rootDirNoSymlinks(root *os.Root, rel string, create, durable bool) (*os.Root, error) {
	if rel == "" || rel == "." || filepath.IsAbs(rel) || filepath.Clean(rel) != rel {
		return nil, fmt.Errorf("packstore: invalid root-relative directory %q", rel)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	parent := root
	parentOwned := false
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			if parentOwned {
				_ = parent.Close()
			}
			return nil, fmt.Errorf("packstore: invalid root-relative directory %q", rel)
		}
		before, err := parent.Lstat(part)
		if create && errors.Is(err, fs.ErrNotExist) {
			if err := parent.Mkdir(part, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				if parentOwned {
					_ = parent.Close()
				}
				return nil, err
			}
			if durable {
				if err := syncFilesystemRootDir(parent); err != nil {
					if parentOwned {
						_ = parent.Close()
					}
					return nil, err
				}
			}
			before, err = parent.Lstat(part)
		}
		if err != nil {
			if parentOwned {
				_ = parent.Close()
			}
			return nil, err
		}
		if before == nil {
			if parentOwned {
				_ = parent.Close()
			}
			return nil, fmt.Errorf("packstore: filesystem directory %q has no identity", part)
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			if parentOwned {
				_ = parent.Close()
			}
			return nil, fmt.Errorf("packstore: unsafe filesystem directory %q", part)
		}
		child, err := parent.OpenRoot(part)
		if err != nil {
			if parentOwned {
				_ = parent.Close()
			}
			return nil, err
		}
		held, heldErr := child.Stat(".")
		after, afterErr := parent.Lstat(part)
		if heldErr != nil || afterErr != nil || held == nil || after == nil || after.Mode()&os.ModeSymlink != 0 ||
			!after.IsDir() || !os.SameFile(before, held) || !os.SameFile(held, after) {
			closeErr := child.Close()
			if parentOwned {
				closeErr = errors.Join(closeErr, parent.Close())
			}
			return nil, errors.Join(
				fmt.Errorf("packstore: filesystem directory %q changed while opening it", part),
				heldErr, afterErr, closeErr,
			)
		}
		if parentOwned {
			if err := parent.Close(); err != nil {
				return nil, errors.Join(err, child.Close())
			}
		}
		parent = child
		parentOwned = true
	}
	return parent, nil
}

func createRootTemp(dir *os.Root, prefix string) (*os.File, string, error) {
	for range 100 {
		name := prefix + pack.NewPackID()
		file, err := dir.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("packstore: exhausted private staging name attempts: %w", fs.ErrExist)
}

func openRootRegularFile(dir *os.Root, name, displayPath string) (*os.File, error) {
	return openRootRegularFileMode(dir, name, displayPath, false)
}

func openRootRegularFileMode(
	dir *os.Root,
	name string,
	displayPath string,
	writable bool,
) (*os.File, error) {
	before, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := validateRegularNoFollow(displayPath, before); err != nil {
		return nil, err
	}
	var file *os.File
	if writable {
		file, err = dir.OpenFile(name, os.O_RDWR, 0)
	} else {
		file, err = dir.Open(name)
	}
	if err != nil {
		return nil, err
	}
	held, heldErr := file.Stat()
	after, afterErr := dir.Lstat(name)
	if heldErr != nil || afterErr != nil || !os.SameFile(before, held) ||
		!os.SameFile(held, after) {
		return nil, errors.Join(
			fmt.Errorf("packstore: filesystem file %q changed while opening it", name),
			heldErr, afterErr, file.Close(),
		)
	}
	if err := validateRegularNoFollow(displayPath, after); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func removeRootRegularFile(dir *os.Root, name, displayPath string) error {
	info, err := dir.Lstat(name)
	if err != nil {
		return err
	}
	if err := validateRegularNoFollow(displayPath, info); err != nil {
		return err
	}
	return dir.Remove(name)
}

func syncRootDir(dir *os.Root) error {
	return syncRootDirPlatform(dir)
}

func missingFilesystemRoot(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
