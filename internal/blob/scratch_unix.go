//go:build darwin || linux

package blob

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func availableScratchBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("stat temporary filesystem: %w", err)
	}
	if stat.Bsize <= 0 {
		return 0, fmt.Errorf("stat temporary filesystem: invalid block size %d", stat.Bsize)
	}
	// #nosec G115 -- Bsize is checked positive immediately above.
	return scratchCapacityBytes(stat.Bavail, uint64(stat.Bsize)), nil
}
