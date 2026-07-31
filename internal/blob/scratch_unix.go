//go:build darwin || linux

package blob

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func availableScratchBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("stat temporary filesystem: %w", err)
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}
