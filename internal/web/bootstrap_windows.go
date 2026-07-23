//go:build windows

package web

import (
	"fmt"

	"go.kenn.io/docbank/internal/winsecurity"
)

func restrictBootstrapFile(path string) error {
	if err := winsecurity.RestrictCurrentUserFile(path); err != nil {
		return fmt.Errorf("securing web bootstrap: %w", err)
	}
	return nil
}
