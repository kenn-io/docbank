//go:build linux || darwin

package filepublish

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// publishRootNoReplace falls back to a hard link where the filesystem rejects
// no-replace renames, as NFS and some FUSE mounts do. A link also refuses to
// replace an existing name; RootStage.Cleanup removes the stage name.
func publishRootNoReplace(source *os.File, name string, destination *os.File, target string, rename func(int, string, int, string) error) error {
	err := rename(int(source.Fd()), name, int(destination.Fd()), target)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	if linkErr := unix.Linkat(int(source.Fd()), name, int(destination.Fd()), target, 0); linkErr != nil {
		return errors.Join(err, &os.LinkError{Op: "linkat", Old: name, New: target, Err: linkErr})
	}
	return nil
}
