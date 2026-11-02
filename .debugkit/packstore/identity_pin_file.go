package packstore

import "os"

type fileIdentityPin struct {
	*os.File
}

func (p *fileIdentityPin) removeClaim(path string) error {
	return removeFileIdentityPinClaim(p.File, path)
}
