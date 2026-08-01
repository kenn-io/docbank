//go:build windows

package config

import (
	"fmt"

	"go.kenn.io/docbank/internal/winsecurity"
)

func restrictWrittenConfig(path string) error {
	if err := winsecurity.RestrictCurrentUserFile(path); err != nil {
		return fmt.Errorf("securing restored config.toml: %w", err)
	}
	return nil
}
