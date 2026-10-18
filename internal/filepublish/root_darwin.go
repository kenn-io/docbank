//go:build darwin

package filepublish

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	return publishRootNoReplace(source, name, destination, target, func(source int, name string, destination int, target string) error {
		return unix.RenameatxNp(source, name, destination, target, unix.RENAME_EXCL)
	})
}
