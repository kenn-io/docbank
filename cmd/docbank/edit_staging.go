package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type privateStaging struct {
	path       string
	root       *os.Root
	pin        *os.File
	stagedName string
}

func openPrivateStaging(path string, pin *os.File) (*privateStaging, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		var pinErr error
		if pin != nil {
			pinErr = pin.Close()
		}
		return nil, errors.Join(
			fmt.Errorf("opening private edit staging: %w", err),
			pinErr,
			os.RemoveAll(path),
		)
	}
	return &privateStaging{path: path, root: root, pin: pin}, nil
}

func (s *privateStaging) createFile(vaultName string) (*os.File, string, error) {
	pattern := editStagePattern(vaultName)
	for range 10 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("generating staged filename: %w", err)
		}
		component := strings.Replace(pattern, "*", hex.EncodeToString(random[:]), 1)
		file, err := s.root.OpenFile(component, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("creating staged file: %w", err)
		}
		s.stagedName = component
		return file, filepath.Join(s.path, component), nil
	}
	return nil, "", errors.New("creating staged file: repeated name collisions")
}

func (s *privateStaging) removeAll() error {
	var removeFileErr error
	if s.root != nil && s.stagedName != "" {
		removeFileErr = s.root.Remove(s.stagedName)
		if errors.Is(removeFileErr, os.ErrNotExist) {
			removeFileErr = nil
		}
	}
	var rootErr error
	if s.root != nil {
		rootErr = s.root.Close()
		s.root = nil
	}
	var pinErr error
	if s.pin != nil {
		pinErr = s.pin.Close()
		s.pin = nil
	}
	return errors.Join(removeFileErr, rootErr, pinErr, os.RemoveAll(s.path))
}
