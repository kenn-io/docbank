//go:build windows

package home

import (
	"errors"
	"fmt"
	"os"
)

func secureOptionalConfig(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return enforceMode(path, 0o600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	return nil
}
