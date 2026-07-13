//go:build windows

package home

import (
	"fmt"
	"os"

	"go.kenn.io/kit/safefileio"
)

func enforceMode(path string, _ os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking permissions of %s: %w", path, err)
	}
	if info.IsDir() {
		if err := safefileio.EnsurePrivateDir(path); err != nil {
			return fmt.Errorf("securing directory %s: %w", path, err)
		}
		return nil
	}
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return fmt.Errorf("validating private file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing private file %s: %w", path, err)
	}
	return nil
}
