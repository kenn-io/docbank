//go:build linux

package filepublish

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	return unix.Renameat2(int(source.Fd()), name, int(destination.Fd()), target, unix.RENAME_NOREPLACE)
}
