package filepublish

import (
	"errors"
	"os"
	"path/filepath"
)

const rootStagePrefix = ".docbank-checkout-"

// RootStage keeps staging and publication beneath held directory handles.
type RootStage struct {
	File          *os.File
	parent        *os.Root
	stage         *os.Root
	pin           *os.File
	component     string
	directory     string
	syncDirectory func(*os.Root, string) error
}

func CreateRootStage(root *os.Root, parent string) (*RootStage, error) {
	held, err := root.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	name, err := newStageName(rootStagePrefix)
	if err != nil {
		_ = held.Close()
		return nil, err
	}
	pin, err := makePrivateRootStage(held, name)
	if err != nil {
		_ = held.Close()
		return nil, err
	}
	s := &RootStage{parent: held, pin: pin, component: name, directory: parent, syncDirectory: syncRootDirectory}
	s.stage, err = held.OpenRoot(name)
	if err == nil {
		s.File, err = s.stage.OpenFile("payload.tmp", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	}
	if err != nil {
		_ = s.Cleanup()
		return nil, err
	}
	return s, nil
}

func (s *RootStage) Publish(destination string) (bool, error) {
	if filepath.Dir(destination) != s.directory {
		return false, errors.New("root stage must share the destination directory")
	}
	source, err := s.stage.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = source.Close() }()
	target, err := s.parent.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = target.Close() }()
	if err := renameRootNoReplace(source, "payload.tmp", target, filepath.Base(destination)); err != nil {
		return false, err
	}
	return true, s.syncDirectory(s.parent, ".")
}

func (s *RootStage) Cleanup() error {
	var err error
	if s.File != nil {
		err = s.File.Close()
		s.File = nil
	}
	if s.stage != nil {
		removeErr := s.stage.Remove("payload.tmp")
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
		err = errors.Join(err, s.stage.Close())
		s.stage = nil
	}
	if s.pin != nil {
		err = errors.Join(err, s.pin.Close())
		s.pin = nil
	}
	if s.parent != nil {
		removeErr := s.parent.Remove(s.component)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
		err = errors.Join(err, s.parent.Close())
		s.parent = nil
	}
	return err
}

// CleanupRootStages removes only exact stage names containing only a regular payload.tmp.
func CleanupRootStages(root *os.Root, parent string) error {
	held, err := root.OpenRoot(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = held.Close() }()
	dir, err := held.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	err = errors.Join(err, dir.Close())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isStageName(entry.Name(), rootStagePrefix) {
			continue
		}
		stage, err := held.OpenRoot(entry.Name())
		if err != nil {
			return err
		}
		dir, err := stage.Open(".")
		if err != nil {
			_ = stage.Close()
			return err
		}
		children, readErr := dir.ReadDir(-1)
		readErr = errors.Join(readErr, dir.Close())
		if readErr != nil {
			_ = stage.Close()
			return readErr
		}
		if len(children) == 0 || len(children) == 1 && children[0].Name() == "payload.tmp" && children[0].Type().IsRegular() {
			if len(children) == 1 {
				err = stage.Remove("payload.tmp")
			}
			err = errors.Join(err, stage.Close())
			if err == nil {
				err = held.Remove(entry.Name())
			}
		} else {
			err = stage.Close()
		}
		if err != nil {
			return err
		}
	}
	return nil
}
