//go:build !windows

package ingest

import "os"

func openWatchLeaf(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
