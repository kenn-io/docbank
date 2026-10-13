//go:build !windows

package filepublish

import (
	"errors"
	"os"
)

func createPrivateRootFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
}

func syncRootDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func createPrivateRootDirectory(root *os.Root, name string) error { return root.Mkdir(name, 0700) }
