package safefileio

import (
	"errors"
	"os"

	"go.kenn.io/kit/fsname"
)

// verifyParentAccessPolicy rejects a parent directory whose file system or
// macOS extended ACL can grant access that its mode bits do not show.
func verifyParentAccessPolicy(dir *os.File) error {
	if err := rejectRemoteParent(dir); err != nil {
		return err
	}
	return validateDarwinExtendedACL(dir)
}

func rejectRemoteParent(dir *os.File) error {
	remote, err := fsname.RemoteFile(dir)
	if err != nil {
		return err
	}
	if remote {
		return errors.New("parent is on a network or user-space file system")
	}
	return nil
}
