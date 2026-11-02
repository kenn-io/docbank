//go:build windows

package packstore

import "io/fs"

// Windows restoration must not request DELETE access: ordinary readers and
// writers do not share deletion through os.Open, so such a handle would block
// existing writers from continuing to update the exact inode being restored.
func openLooseRestorationIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	file, identity, err := openLooseRepairPin(path)
	if err != nil {
		return nil, nil, err
	}
	return &fileIdentityPin{File: file}, identity, nil
}
