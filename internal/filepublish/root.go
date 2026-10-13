package filepublish

import (
	"errors"
	"os"
	"path/filepath"
)

const rootStagePrefix = ".docbank-checkout-"

type RootStage struct {
	File          *os.File
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

func (s *RootStage) Publish(destination string) (bool, error) {
	if !filepath.IsLocal(destination) {
		return false, errors.New("checkout destination must be root-relative")
	}
	parent, err := s.root.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return false, err
	}
	defer func() { _ = parent.Close() }()
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
	if err := renameRootNoReplace(source, s.name, target, filepath.Base(destination)); err != nil {
		return false, err
	}
	installed, err := s.root.Lstat(destination)
	if err != nil {
		return false, err
	}
	if !installed.Mode().IsRegular() || !os.SameFile(s.info, installed) {
		return false, errors.New("checkout destination changed during publication")
	}
	return true, s.syncDirectory(parent, ".")
}

func (s *RootStage) Cleanup() error {
	var err error
	if s.File != nil {
		err = s.File.Close()
		s.File = nil
	}
	removeErr := s.root.Remove(s.name)
	if !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

// CleanupRootStages removes only exact regular stage files at the checkout root.
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
		if entry.Type().IsRegular() && isStageName(entry.Name(), rootStagePrefix) {
			if err := root.Remove(entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}
