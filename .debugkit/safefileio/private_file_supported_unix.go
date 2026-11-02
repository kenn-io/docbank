//go:build darwin || linux

package safefileio

import (
	"fmt"
	"os"
)

// privateFileCreationSupported reports that private files can be verified
// here, so CreatePrivateFile may create one.
func privateFileCreationSupported() error { return nil }

func validatePrivateCurrentUserFile(
	file *os.File,
	validatePlatformAccess func(*os.File) error,
) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	if err := verifyPrivateFileMode(file); err != nil {
		return err
	}
	return validatePlatformAccess(file)
}

func verifyPrivateFileMode(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		return fmt.Errorf("safefileio: file mode is %04o, not 0600", mode)
	}
	return nil
}
