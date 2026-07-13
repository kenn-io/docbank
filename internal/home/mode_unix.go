//go:build !windows

package home

import (
	"fmt"
	"os"
)

func enforceMode(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking permissions of %s: %w", path, err)
	}
	if info.Mode().Perm() == mode {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("tightening permissions of %s: %w", path, err)
	}
	return nil
}
