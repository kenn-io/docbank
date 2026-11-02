//go:build !windows

package atomicfile

import (
	"fmt"
	"os"
)

// SyncDir fsyncs the directory dir so recent changes to its entries (a
// rename, a new file) survive a crash. POSIX allows opening a directory
// read-only and syncing the resulting descriptor.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("atomicfile: open directory %s for sync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("atomicfile: sync directory %s: %w", dir, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("atomicfile: close directory %s after sync: %w", dir, err)
	}
	return nil
}
