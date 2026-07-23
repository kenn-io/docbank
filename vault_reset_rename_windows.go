//go:build windows

package docbank

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
	"golang.org/x/sys/windows"
)

func renameVaultNoReplace(source, destination string) error {
	return renameVaultNoReplaceWithMove(source, destination, windows.MoveFile)
}

func renameVaultNoReplaceWithMove(
	source, destination string,
	move func(*uint16, *uint16) error,
) error {
	extendedSource, err := winsecurity.ExtendedLengthPath(source)
	if err != nil {
		return err
	}
	extendedDestination, err := winsecurity.ExtendedLengthPath(destination)
	if err != nil {
		return err
	}
	sourceName, err := windows.UTF16PtrFromString(extendedSource)
	if err != nil {
		return err
	}
	destinationName, err := windows.UTF16PtrFromString(extendedDestination)
	if err != nil {
		return err
	}
	if err := move(sourceName, destinationName); err != nil {
		return &os.LinkError{Op: "MoveFileW", Old: source, New: destination, Err: err}
	}
	return nil
}
