//go:build unix

package ingest

import (
	"os"
	"syscall"
)

// openNoFollow opens src refusing to traverse a final-component symlink, so
// the file classified by Lstat/WalkDir is the file that gets read even if it
// was swapped in between. O_NONBLOCK keeps a swapped-in FIFO from hanging
// the open; it has no effect on regular-file I/O afterwards.
func openNoFollow(src string) (*os.File, error) {
	return os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
