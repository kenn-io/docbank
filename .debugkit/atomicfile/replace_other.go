//go:build !windows

package atomicfile

import "os"

// Replace renames oldpath to newpath, atomically replacing an existing file
// at newpath. It never copies, so a rename across volumes fails. A symlink or
// junction at either path is renamed or replaced as an entry, never followed.
// A directory at newpath is never replaced: Replace fails with an error
// wrapping fs.ErrExist, as os.Rename does. That is a check just before the
// rename, not an atomic guarantee.
//
// On Unix it is os.Rename; rename(2) fails with EXDEV across devices. On
// other platforms it is also os.Rename, which is not atomic everywhere: on
// Plan 9 it removes an existing target before renaming. On
// Windows it first renames with POSIX semantics, which succeeds while another
// handle holds newpath open with FILE_SHARE_DELETE (the holder keeps reading
// the old content), and falls back to MoveFileEx where Windows or the file
// system does not support that rename. A handle opened there without
// FILE_SHARE_DELETE, as os.Open opens files, still makes Replace fail.
func Replace(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath) //nolint:forbidigo // Replace is os.Rename off Windows
}
