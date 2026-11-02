//go:build !windows

package atomicfile

// publishNew publishes a WriteNew staging file at final without replacing an
// existing entry.
func publishNew(staging, final string) error {
	return PublishNoReplace(staging, final)
}
