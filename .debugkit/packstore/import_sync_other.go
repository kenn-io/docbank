//go:build !windows

package packstore

import (
	"fmt"
	"os"
)

func syncImportRootDirPlatform(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("open rooted directory for sync: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync rooted directory: %w", err)
	}
	return nil
}
