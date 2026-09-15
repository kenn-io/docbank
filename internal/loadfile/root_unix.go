//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package loadfile

import (
	"os"
	"syscall"
)

func openRootRegular(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
