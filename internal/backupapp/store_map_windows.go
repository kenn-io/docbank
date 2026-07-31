//go:build windows

package backupapp

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
)

func openRestoreStoreMap(path string) (*os.File, error) {
	return winsecurity.OpenRestrictedCurrentUserFile(path)
}
