//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/safefileio"

	"go.kenn.io/docbank/internal/winsecurity"
)

func makePrivateEditDir() (string, error) {
	return makePrivateEditDirAt(os.TempDir())
}

func makePrivateEditDirAt(parentPath string) (string, error) {
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return "", fmt.Errorf("opening edit staging parent: %w", err)
	}
	defer func() { _ = parent.Close() }()

	for range 10 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("generating edit staging name: %w", err)
		}
		component := "docbank-edit-" + hex.EncodeToString(random)
		if err := winsecurity.MkdirPrivateAt(parent, component); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", fmt.Errorf("creating private edit staging: %w", err)
		}
		path := filepath.Join(parentPath, component)
		if err := safefileio.ValidatePrivateDir(path); err != nil {
			cleanupErr := os.RemoveAll(path)
			return "", errors.Join(
				fmt.Errorf("validating private edit staging: %w", err),
				cleanupErr,
			)
		}
		return path, nil
	}
	return "", errors.New("creating private edit staging: repeated name collisions")
}
