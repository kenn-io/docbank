//go:build !windows

package home

import "os"

func mkdirVaultDir(parent *os.Root, component string) error {
	return parent.Mkdir(component, 0o700)
}
