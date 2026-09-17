//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package loadfile

import "os"

func openRootRegular(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
