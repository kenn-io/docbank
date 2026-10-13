//go:build linux

package filepublish

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	if err := unix.Renameat2(int(source.Fd()), name, int(destination.Fd()), target, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publishing checkout file without replacement: %w", err)
	}
	return nil
}
