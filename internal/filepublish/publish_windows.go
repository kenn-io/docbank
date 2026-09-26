//go:build windows

package filepublish

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func windowsPaths(stagedPath, destinationPath string) (*uint16, *uint16, error) {
	staged, err := windows.UTF16PtrFromString(stagedPath)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding staged path %q: %w", stagedPath, err)
	}
	destination, err := windows.UTF16PtrFromString(destinationPath)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding destination path %q: %w", destinationPath, err)
	}
	return staged, destination, nil
}

func renameNoReplace(stagedPath, destinationPath string) error {
	staged, destination, err := windowsPaths(stagedPath, destinationPath)
	if err == nil {
		err = windows.MoveFile(staged, destination)
	}
	if err != nil {
		return &os.LinkError{Op: "MoveFileW", Old: stagedPath, New: destinationPath, Err: err}
	}
	return nil
}

func replaceFile(stagedPath, destinationPath string) error {
	staged, destination, err := windowsPaths(stagedPath, destinationPath)
	if err == nil {
		err = windows.MoveFileEx(staged, destination, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	}
	if err != nil {
		return &os.LinkError{Op: "MoveFileExW", Old: stagedPath, New: destinationPath, Err: err}
	}
	return nil
}
