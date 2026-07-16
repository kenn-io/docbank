//go:build !windows

package main

import "os"

func makePrivateEditDir() (string, error) {
	return os.MkdirTemp("", "docbank-edit-*")
}
