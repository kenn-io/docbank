//go:build !windows

package spill_test

import "os"

// openShared opens the list's file for writing while the list holds it.
func openShared(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}
