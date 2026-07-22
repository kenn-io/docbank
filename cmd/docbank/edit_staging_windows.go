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

func makePrivateEditDir() (*privateStaging, error) {
	return makePrivateEditDirAt(os.TempDir())
}

func makePrivateEditDirAt(parentPath string) (*privateStaging, error) {
	return makePrivateStagingDirAt(parentPath, "docbank-edit-")
}

func makePrivateStagingDirAt(parentPath, prefix string) (*privateStaging, error) {
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, fmt.Errorf("opening edit staging parent: %w", err)
	}
	defer func() { _ = parent.Close() }()

	for range 10 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, fmt.Errorf("generating edit staging name: %w", err)
		}
		component := prefix + hex.EncodeToString(random)
		pin, err := winsecurity.MkdirPrivatePinnedAt(parent, component)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("creating private edit staging: %w", err)
		}
		path := filepath.Join(parentPath, component)
		if err := safefileio.ValidatePrivateDir(path); err != nil {
			pinErr := pin.Close()
			cleanupErr := os.RemoveAll(path)
			return nil, errors.Join(
				fmt.Errorf("validating private edit staging: %w", err),
				pinErr,
				cleanupErr,
			)
		}
		return openPrivateStaging(path, pin)
	}
	return nil, errors.New("creating private edit staging: repeated name collisions")
}
