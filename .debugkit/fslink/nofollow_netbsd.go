package fslink

import "syscall"

// noFollowErrnos are the errnos O_NOFOLLOW produces for a final-component
// symlink. NetBSD reports EFTYPE.
var noFollowErrnos = []syscall.Errno{syscall.ELOOP, syscall.EMLINK, syscall.EFTYPE}
