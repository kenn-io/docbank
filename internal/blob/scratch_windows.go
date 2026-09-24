//go:build windows

package blob

import (
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

func availableScratchBytes(path string) (int64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encoding %s: %w", path, err)
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, nil, nil); err != nil {
		return 0, fmt.Errorf("reading free space for %s: %w", path, err)
	}
	if available > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}
