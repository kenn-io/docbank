//go:build windows

package qmdexport

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"go.kenn.io/docbank/internal/winsecurity"
)

func restrictNewFile(path string) error {
	if err := winsecurity.RestrictCurrentUserFile(path); err != nil {
		return fmt.Errorf("secure qmd export file: %w", err)
	}
	return nil
}

func openPrivateFile(path string) (*os.File, error) {
	file, err := winsecurity.OpenRestrictedCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("open private qmd export file: %w", err)
	}
	return file, nil
}

// Windows has no directory fsync; request write-through for publication renames.
func renamePublished(source, destination string) error {
	from, err := winsecurity.ExtendedLengthPath(source)
	if err != nil {
		return fmt.Errorf("resolve qmd export rename path: %w", err)
	}
	to, err := winsecurity.ExtendedLengthPath(destination)
	if err != nil {
		return fmt.Errorf("resolve qmd export rename path: %w", err)
	}
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return fmt.Errorf("resolve qmd export rename path: %w", err)
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return fmt.Errorf("resolve qmd export rename path: %w", err)
	}
	if err := windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: source, New: destination, Err: err}
	}
	return nil
}
