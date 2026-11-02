//go:build !unix && !windows

package packstore

import "os"

func removeFileIdentityPinClaim(_ *os.File, path string) error {
	return removeLooseCanonicalFile(path)
}
