//go:build windows

package mistral

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func availableDiskBytes(path string) (int64, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encoding %s: %w", path, err)
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pathUTF16, &available, nil, nil); err != nil {
		return 0, fmt.Errorf("reading free space for %s: %w", path, err)
	}
	const maxInt64 = ^uint64(0) >> 1
	if available > maxInt64 {
		return int64(maxInt64), nil
	}
	return int64(available), nil
}
