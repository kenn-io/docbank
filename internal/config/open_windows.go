//go:build windows

package config

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
)

func openConfig(path string) (*os.File, error) {
	return winsecurity.OpenRestrictedCurrentUserFile(path)
}
