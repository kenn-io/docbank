//go:build !darwin && !linux && !windows

package filepublish

import (
	"errors"
	"os"
)

func renameNoReplace(stagedPath, destinationPath string) error {
	return &os.LinkError{Op: "rename-noreplace", Old: stagedPath, New: destinationPath, Err: errors.ErrUnsupported}
}
