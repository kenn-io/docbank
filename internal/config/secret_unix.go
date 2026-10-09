//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kit/safefileio"
)

// Open without following symlinks or blocking on FIFOs. Validate the opened
// handle, so replacing the path cannot bypass ownership or mode checks.
func openSecret(path string) (*os.File, error) {
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening secret file: %w", err)
	}
	info, err := file.Stat()
	if err != nil || (info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600) {
		_ = file.Close()
		return nil, errors.New("secret must have mode 0400 or 0600")
	}
	return file, nil
}
