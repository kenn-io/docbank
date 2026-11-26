//go:build !windows

package store

func isTemporaryWriteSharingError(string, error) bool {
	return false
}
