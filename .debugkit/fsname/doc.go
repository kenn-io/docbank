// Package fsname checks and cleans file and directory names so they work on
// every modern operating system and file system, not just the host's.
//
// Use Check to reject a name a caller supplied, Clean to derive a usable name
// from an outside string such as an attachment or export title, and Join to
// build a root-relative path from untrusted parts for fslink.OpenInRoot.
// Check never renames: a name that is not portable is an error. The rules come
// from github.com/spf13/pathologize, plus Windows console device names it does
// not yet cover. Check, Clean, Join and CheckPath are lexical and never touch
// the file system.
//
// Remote and RemoteFile are the only functions that touch the file system.
// They report where a file system lives, not whether a path is portable:
// whether it is a network or user-space (FUSE) file system whose access
// checks, locking and durability the local kernel does not enforce. Callers
// holding private runtime state, lock files or sockets should refuse remote
// file systems; general data writes may allow them. The check is best
// effort: it recognizes common network and FUSE file systems, and on
// platforms with no detection method it fails with an error wrapping
// errors.ErrUnsupported.
package fsname
