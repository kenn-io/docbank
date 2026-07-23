//go:build !darwin && !linux && !windows

package docbank

import (
	"errors"
	"os"
)

func renameVaultNoReplace(source, destination string) error {
	return &os.LinkError{
		Op: "rename-noreplace", Old: source, New: destination, Err: errors.ErrUnsupported,
	}
}
