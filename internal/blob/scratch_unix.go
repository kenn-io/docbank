//go:build darwin || linux

package blob

import (
	"errors"
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
	if available == 0 {
		return 0, errors.New("temporary filesystem reports no available space")
	}
	return int64(available), nil
}
