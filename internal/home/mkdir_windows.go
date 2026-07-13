//go:build windows

package home

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
)

func mkdirVaultDir(parent *os.Root, component string) error {
	return winsecurity.MkdirPrivateAt(parent, component)
}
