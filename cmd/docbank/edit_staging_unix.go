//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

func makePrivateEditDir() (*privateStaging, error) {
	return makePrivateStagingDirAt(os.TempDir(), "docbank-edit-")
}

func makePrivateStagingDirAt(parentPath, prefix string) (*privateStaging, error) {
	path, err := os.MkdirTemp(parentPath, prefix+"*")
	if err != nil {
		return nil, err
	}
	return openPrivateStaging(filepath.Clean(path), nil)
}
