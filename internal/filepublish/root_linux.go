//go:build linux

package filepublish

import "os"

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	return renameAtNoReplace(int(source.Fd()), name, int(destination.Fd()), target)
}
