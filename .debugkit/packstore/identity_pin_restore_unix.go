//go:build unix

package packstore

import "io/fs"

func openLooseRestorationIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	return openLooseIdentityPin(path)
}
