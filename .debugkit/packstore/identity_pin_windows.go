//go:build windows

package packstore

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/windows"
)

// Windows removal pins request delete access up front so cleanup can mark the
// exact claimed handle for deletion instead of reopening a pathname after
// verification. Repair pins remain zero-access for ReplaceFileW compatibility.
func openLooseRemovalIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	file, err := openWindowsNoFollow(path, windows.DELETE)
	if err != nil {
		return nil, nil, err
	}
	identity, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	if err := validateRegularNoFollow(path, identity); err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return &fileIdentityPin{File: file}, identity, nil
}
