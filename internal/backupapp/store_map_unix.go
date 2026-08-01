//go:build !windows

package backupapp

import (
	"errors"
	"os"
	"syscall"

	"go.kenn.io/docbank/internal/blob"
)

func openRestoreStoreMap(path string) (*os.File, error) {
	file, err := blob.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		_ = file.Close()
		return nil, errors.New("cannot determine restore store-map ownership")
	}
	if int(stat.Uid) != os.Getuid() {
		_ = file.Close()
		return nil, errors.New("restore store map is not owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, errors.New("restore store map permissions must be 0600 or stricter")
	}
	return file, nil
}
