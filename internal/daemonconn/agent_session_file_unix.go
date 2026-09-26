//go:build !windows

package daemonconn

import "os"

func openPrivateAgentSessionFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 {
		return nil, ErrAgentSessionFile
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrAgentSessionFile
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		after.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, ErrAgentSessionFile
	}
	return file, nil
}
