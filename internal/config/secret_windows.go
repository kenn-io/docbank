package config

import (
	"fmt"
	"os"

	"go.kenn.io/kit/safefileio"
)

func openSecret(path string) (*os.File, error) {
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening secret file: %w", err)
	}
	// Accept owner-only read access as well as writable files. The native
	// handle checks reject reparse points, foreign owners, and broad DACLs.
	if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("checking secret file access: %w", err)
	}
	return file, nil
}
