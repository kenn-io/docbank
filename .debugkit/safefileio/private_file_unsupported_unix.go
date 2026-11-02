//go:build unix && !darwin && !linux

package safefileio

import (
	"fmt"
	"os"
	"runtime"
)

// ValidatePrivateCurrentUserFile fails closed on Unix platforms where Kit
// cannot inspect access-control lists through the verified file handle.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	return fmt.Errorf(
		"safefileio: private current-user file validation is unsupported on %s",
		runtime.GOOS,
	)
}

// privateFileCreationSupported fails, because ValidatePrivateCurrentUserFile
// cannot pass here and a created file would only be left behind.
func privateFileCreationSupported() error {
	return fmt.Errorf(
		"safefileio: private current-user file validation is unsupported on %s",
		runtime.GOOS,
	)
}

// verifyParentAccessPolicy is unreachable here, since creation fails first,
// and refuses in case that changes.
func verifyParentAccessPolicy(*os.File) error {
	return privateFileCreationSupported()
}
