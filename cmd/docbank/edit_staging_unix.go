//go:build !windows

package main

import "os"

func makePrivateEditDir() (*editStaging, error) {
	path, err := os.MkdirTemp("", "docbank-edit-*")
	if err != nil {
		return nil, err
	}
	return openEditStaging(path, nil)
}
