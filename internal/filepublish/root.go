package filepublish

import (
	"errors"
	"os"
	"path/filepath"
)

// RootStage keeps every filesystem operation beneath the held checkout root.
type RootStage struct {
	File *os.File
	root *os.Root
	name string
	dir  string
}

func CreateRootStage(root *os.Root, parent string) (*RootStage, error) {
	name, err := newStageName(".docbank-checkout-")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, name)
	if err := root.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	stage := &RootStage{root: root, dir: dir, name: filepath.Join(dir, "payload.tmp")}
	stage.File, err = root.OpenFile(stage.name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = stage.Cleanup()
		return nil, err
	}
	return stage, nil
}

func (s *RootStage) Publish(destination string) (bool, error) {
	if filepath.Dir(destination) != filepath.Dir(s.dir) {
		return false, errors.New("root stage must share the destination directory")
	}
	if err := s.root.Link(s.name, destination); err != nil {
		return false, err
	}
	return true, syncRootDirectory(s.root, filepath.Dir(destination))
}

func (s *RootStage) Cleanup() error {
	if s.File != nil {
		_ = s.File.Close()
		s.File = nil
	}
	fileErr := s.root.Remove(s.name)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	dirErr := s.root.Remove(s.dir)
	if errors.Is(dirErr, os.ErrNotExist) {
		dirErr = nil
	}
	return errors.Join(fileErr, dirErr)
}
