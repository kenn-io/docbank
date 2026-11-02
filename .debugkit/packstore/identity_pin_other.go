//go:build !unix && !windows

package packstore

import "io/fs"

// Unknown platforms retain the existing no-follow readable pin and fail
// closed when the file cannot be opened safely.
func openLooseRemovalIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	file, identity, err := openLooseRepairPin(path)
	if err != nil {
		return nil, nil, err
	}
	return &fileIdentityPin{File: file}, identity, nil
}
