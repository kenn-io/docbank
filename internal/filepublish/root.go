package filepublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const rootStagePrefix = ".docbank-checkout-"

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

func (s *RootStage) Publish(destination string) (bool, error) {
	if !filepath.IsLocal(destination) {
		return false, errors.New("checkout destination must be root-relative")
	}
	parent, err := s.root.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return false, err
	}
	defer func() { _ = parent.Close() }()
	return s.publish(parent, destination)
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

func (s *RootStage) publish(parent *os.Root, destination string) (bool, error) {
	installed, err := s.install(parent, destination)
	if !installed {
		return false, err
	}
	return true, errors.Join(err, s.syncDirectory(parent, "."))
}

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

// CheckRootPaths probes the destination's own name-equivalence rules before copying bytes.
func CheckRootPaths(ctx context.Context, root *os.Root, paths []string) (retErr error) {
	type probe struct {
		path string
		info os.FileInfo
	}
	var probes []probe
	defer func() {
		for _, probe := range probes {
			current, err := root.Lstat(probe.path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err == nil && !os.SameFile(probe.info, current) {
				err = errors.New("checkout path changed during validation")
			}
			if err == nil {
				err = root.Remove(probe.path)
			}
			retErr = errors.Join(retErr, err)
		}
	}()
	stage := RootStage{root: root, syncDirectory: syncRootDirectory}
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stage.MakeParentDirectories(name); err != nil {
			return err
		}
		probeStage, err := CreateRootStage(root)
		if err != nil {
			return err
		}
		installed, installErr := probeStage.Install(name)
		if installed {
			probes = append(probes, probe{name, probeStage.info})
		} else if installErr != nil {
			if _, err := root.Lstat(name); err == nil {
				installErr = errors.Join(os.ErrExist, installErr)
			}
		}
		if err := errors.Join(installErr, probeStage.Cleanup()); err != nil {
			return &os.PathError{Op: "reserving checkout path", Path: name, Err: err}
		}
	}
	return nil
}
