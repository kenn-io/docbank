//go:build !windows

package packstore

import (
	"errors"
	"os"
)

func syncRootDirPlatform(dir *os.Root) error {
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
