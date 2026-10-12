//go:build !darwin && !linux && !windows

package filepublish

import (
	"errors"
	"os"
)

func renameRootNoReplace(_ *os.File, _ string, _ *os.File, _ string) error {
	return errors.ErrUnsupported
}
