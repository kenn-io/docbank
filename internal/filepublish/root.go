package filepublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const rootStagePrefix = ".docbank-checkout-"
const rootProbePrefix = ".docbank-checkout-probe-"

type RootStage struct {
	File          *os.File
	parent        *os.Root
	root          *os.Root
	name          string
	info          os.FileInfo
	syncDirectory func(*os.Root, string) error
}

func CreateRootStage(root *os.Root) (*RootStage, error) {
	name, err := newStageName(rootStagePrefix)
	if err != nil {
		return nil, err
	}
	file, err := createPrivateRootFile(root, name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = root.Remove(name)
		return nil, err
	}
	return &RootStage{File: file, root: root, name: name, info: info, syncDirectory: syncRootDirectory}, nil
}

// MakeParentDirectories makes each ancestor durable before a file can be published.
func (s *RootStage) MakeParentDirectories(destination string) error {
	if !filepath.IsLocal(destination) {
		return errors.New("checkout destination must be root-relative")
	}
	current := "."
	for component := range strings.SplitSeq(filepath.Dir(destination), string(filepath.Separator)) {
		if component == "." {
			continue
		}
		parent := current
		current = filepath.Join(current, component)
		err := createPrivateRootDirectory(s.root, current)
		if errors.Is(err, os.ErrExist) {
			info, err := s.root.Stat(current)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return errors.New("checkout parent is not a directory")
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := s.syncDirectory(s.root, parent); err != nil {
			return err
		}
	}
	return nil
}

// Install publishes without syncing so its owner can record the installed copy first.
func (s *RootStage) Install(destination string) (bool, error) {
	if !filepath.IsLocal(destination) {
		return false, errors.New("checkout destination must be root-relative")
	}
	parent, err := s.root.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return false, err
	}
	installed, err := s.install(parent, destination)
	if installed {
		s.parent = parent
	} else {
		err = errors.Join(err, parent.Close())
	}
	return installed, err
}

func (s *RootStage) SyncParent() error { return s.syncDirectory(s.parent, ".") }

func (s *RootStage) install(parent *os.Root, destination string) (bool, error) {
	source, err := s.root.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = source.Close() }()
	target, err := parent.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = target.Close() }()
	rootInfo, err := s.root.Stat(".")
	if err != nil {
		return false, err
	}
	namedRoot, err := os.Stat(s.root.Name())
	if err != nil {
		return false, err
	}
	heldParent, err := parent.Stat(".")
	if err != nil {
		return false, err
	}
	namedParent, err := s.root.Stat(filepath.Dir(destination))
	if err != nil {
		return false, err
	}
	if !os.SameFile(rootInfo, namedRoot) || !os.SameFile(heldParent, namedParent) {
		return false, errors.New("checkout destination changed before publication")
	}
	if err := renameRootNoReplace(source, s.name, target, filepath.Base(destination)); err != nil {
		return false, err
	}
	installed, err := s.root.Lstat(destination)
	if err != nil {
		return true, err
	}
	if !installed.Mode().IsRegular() || !os.SameFile(s.info, installed) {
		return true, errors.New("checkout destination changed during publication")
	}
	return true, nil
}

func (s *RootStage) Cleanup() error {
	var err error
	if s.parent != nil {
		err = s.parent.Close()
		s.parent = nil
	}
	if s.File != nil {
		err = errors.Join(err, s.File.Close())
		s.File = nil
	}
	removeErr := s.root.Remove(s.name)
	if !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

// CleanupRootStages removes exact stage files and isolated probe directories.
func CleanupRootStages(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	err = errors.Join(err, dir.Close())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && isStageName(entry.Name(), rootProbePrefix) {
			if err := root.RemoveAll(entry.Name()); err != nil {
				return err
			}
		} else if entry.Type().IsRegular() && isStageName(entry.Name(), rootStagePrefix) {
			if err := root.Remove(entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckRootPaths probes name equivalence in an isolated directory on the destination filesystem.
func CheckRootPaths(ctx context.Context, root *os.Root, paths []string) (retErr error) {
	if len(paths) == 0 {
		return ctx.Err()
	}
	name, err := newStageName(rootProbePrefix)
	if err != nil {
		return err
	}
	if err := createPrivateRootDirectory(root, name); err != nil {
		return err
	}
	probes, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, probes.Close()) }()
	info, err := probes.Stat(".")
	if err != nil {
		return err
	}
	defer func() {
		current, err := root.Lstat(name)
		if err == nil && !os.SameFile(info, current) {
			err = errors.New("checkout probe directory changed during validation")
		}
		if err == nil {
			err = root.RemoveAll(name)
		}
		retErr = errors.Join(retErr, err)
	}()
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !filepath.IsLocal(path) {
			return errors.New("checkout destination must be root-relative")
		}
		if err := probes.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, err := probes.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return &os.PathError{Op: "reserving checkout path", Path: path, Err: err}
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}
