//go:build windows

package qmdexport

import (
	"fmt"
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

func restrictNewFile(path string) error {
	if err := winsecurity.RestrictCurrentUserFile(path); err != nil {
		return fmt.Errorf("secure qmd export file: %w", err)
	}
	return nil
}

// createPrivateFile passes an extended-length path so export trees deeper
// than MAX_PATH still work.
func createPrivateFile(path string) (*os.File, error) {
	extended, err := winsecurity.ExtendedLengthPath(path)
	if err != nil {
		return nil, fmt.Errorf("resolve qmd export file path: %w", err)
	}
	return safefileio.CreatePrivateFile(extended) //nolint:wrapcheck // the caller adds context.
}

func openPrivateFile(path string) (*os.File, error) {
	file, err := winsecurity.OpenRestrictedCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("open private qmd export file: %w", err)
	}
	return file, nil
}
