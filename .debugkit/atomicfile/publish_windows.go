//go:build windows

package atomicfile

// publishNew publishes a WriteNew staging file at final without replacing an
// existing entry. It renames rather than hard-linking: CreateHardLink has no
// write-through and SyncDir is a no-op on Windows, so a hard-linked name
// might not be durable, while RenameNoReplace uses MOVEFILE_WRITE_THROUGH.
func publishNew(staging, final string) error {
	return RenameNoReplace(staging, final)
}
