package safefileio

import (
	"errors"
	"os"

	"go.kenn.io/kit/fsname"
)

// verifyParentAccessPolicy rejects a parent directory on a file system whose
// access policy mode bits cannot show. Linux POSIX ACLs need no separate
// check: a named-user or named-group entry that grants write sets the ACL
// mask, which the group bits report, so requireUnsharedParent refuses it.
func verifyParentAccessPolicy(dir *os.File) error {
	remote, err := fsname.RemoteFile(dir)
	if err != nil {
		return err
	}
	if remote {
		return errors.New("parent is on a network or user-space file system")
	}
	return nil
}
