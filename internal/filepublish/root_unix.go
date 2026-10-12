//go:build !windows

package filepublish

import (
	"errors"
	"os"
)

func syncRootDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
