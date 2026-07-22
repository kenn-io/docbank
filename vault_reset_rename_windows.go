//go:build windows

package docbank

import (
	"os"

	"golang.org/x/sys/windows"
)

func renameVaultNoReplace(source, destination string) error {
	sourceName, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationName, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFile(sourceName, destinationName); err != nil {
		return &os.LinkError{Op: "MoveFileW", Old: source, New: destination, Err: err}
	}
	return nil
}
