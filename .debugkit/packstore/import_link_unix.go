//go:build unix

package packstore

import (
	"errors"

	"golang.org/x/sys/unix"
)

func isImportLinkUnsupported(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.ENOSYS)
}
