//go:build linux

package filepublish

import "os"

func renameRootNoReplace(source *os.File, name string, destination *os.File, target string) error {
	return publishRootNoReplace(source, name, destination, target, renameAtNoReplace)
}
