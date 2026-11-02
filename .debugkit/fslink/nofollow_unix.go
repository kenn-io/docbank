//go:build unix && !netbsd

package fslink

import "syscall"

// noFollowErrnos are the errnos O_NOFOLLOW produces for a final-component
// symlink: ELOOP on Linux and Darwin, EMLINK on FreeBSD.
var noFollowErrnos = []syscall.Errno{syscall.ELOOP, syscall.EMLINK}
