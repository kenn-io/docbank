//go:build !windows

package emailmime

import "os"

func createPrivateSpoolDirectory(parent *os.Root, name string) (*os.File, error) {
	return nil, parent.Mkdir(name, 0o700)
}
