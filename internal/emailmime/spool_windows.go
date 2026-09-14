//go:build windows

package emailmime

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
)

func createPrivateSpoolDirectory(parent *os.Root, name string) (*os.File, error) {
	return winsecurity.MkdirPrivatePinnedAt(parent, name)
}
