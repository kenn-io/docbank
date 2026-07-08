//go:build unix

package blob

import (
	"os"
	"syscall"
)

// OpenNoFollow opens path refusing to traverse a final-component symlink,
// so a classified or hash-addressed file is the file that gets read even if
// it was swapped for a symlink in between. O_NONBLOCK keeps a swapped-in
// FIFO from hanging the open; it has no effect on regular-file I/O
// afterwards.
func OpenNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
