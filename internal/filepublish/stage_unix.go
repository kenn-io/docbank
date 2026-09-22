//go:build !windows

package filepublish

import "os"

func makePrivateStageDir(parent, prefix string) (string, *os.File, error) {
	dir, err := os.MkdirTemp(parent, prefix)
	if err == nil {
		err = os.Chmod(dir, 0o700) //nolint:gosec // Directories require execute permission; no group or other access is granted.
	}
	return dir, nil, err
}
