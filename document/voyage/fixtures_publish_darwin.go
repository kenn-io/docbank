//go:build darwin

package voyage

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func renameFixtureDirectoryNoReplace(
	parentPath, stagingName, destinationName string,
	parentIdentity, stagingIdentity os.FileInfo,
) error {
	parent, err := openVerifiedFixtureParent(parentPath, parentIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := verifyFixtureStagingAt(parent, stagingName, stagingIdentity); err != nil {
		return err
	}
	if err := unix.RenameatxNp(
		int(parent.Fd()), stagingName,
		int(parent.Fd()), destinationName,
		unix.RENAME_EXCL,
	); err != nil {
		return fmt.Errorf("rename fixture directory without replacement: %w", err)
	}
	return nil
}
