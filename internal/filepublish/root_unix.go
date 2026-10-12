//go:build !windows

package filepublish

import (
	"errors"
	"os"
)

func makePrivateRootStage(parent *os.Root, name string) (*os.File, error) {
	return nil, parent.Mkdir(name, 0o700)
}

func syncRootDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
