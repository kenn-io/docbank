//go:build windows

package atomicfile

// SyncDir is a no-op on Windows: unlike POSIX, Windows does not let a caller
// open a directory and fsync the resulting handle to force directory-entry
// changes (renames, new files, new subdirectories) to disk. Durability of the
// file contents still comes from fsyncing the staged file before it is
// renamed into place, and renames are write-through; only the
// directory-entry durability this primitive would add is unavailable here.
func SyncDir(string) error {
	return nil
}
