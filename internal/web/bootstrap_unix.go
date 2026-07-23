//go:build !windows

package web

import (
	"fmt"
	"os"
)

func restrictBootstrapFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("securing web bootstrap: %w", err)
	}
	return nil
}
