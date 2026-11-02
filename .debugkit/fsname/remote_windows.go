package fsname

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// volumeNameDOS is GetFinalPathNameByHandle's VOLUME_NAME_DOS flag (with
// FILE_NAME_NORMALIZED); x/sys/windows does not define it.
const volumeNameDOS = 0x0

func remote(path string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("fsname: resolve %q: %w", path, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return false, fmt.Errorf("fsname: resolve %q: %w", path, err)
	}
	return volumeRemote(abs)
}

// RemoteFile reports whether the open file f lives on a network file system.
// See Remote.
func RemoteFile(f *os.File) (bool, error) {
	final, err := finalPath(windows.Handle(f.Fd()))
	if err != nil {
		return false, fmt.Errorf("fsname: final path of %q: %w", f.Name(), err)
	}
	return volumeRemote(final)
}

// volumeRemote finds the volume root containing the absolute path abs, which
// covers mapped drives, UNC shares and mount folders, and reports whether
// Windows classifies that volume as DRIVE_REMOTE.
func volumeRemote(abs string) (bool, error) {
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return false, fmt.Errorf("fsname: path %q: %w", abs, err)
	}
	buf := make([]uint16, max(len(abs)+2, windows.MAX_PATH+1))
	if err := windows.GetVolumePathName(name, &buf[0], uint32(len(buf))); err != nil {
		return false, fmt.Errorf("fsname: volume of %q: %w", abs, err)
	}
	return windows.GetDriveType(&buf[0]) == windows.DRIVE_REMOTE, nil
}

// finalPath returns the handle's DOS path without the \\?\ prefix that
// GetFinalPathNameByHandle adds, so GetVolumePathName sees C:\... or
// \\server\share\... as it would for a caller-supplied path.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH+1)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), volumeNameDOS)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			path := windows.UTF16ToString(buf[:n])
			if rest, ok := strings.CutPrefix(path, `\\?\UNC\`); ok {
				return `\\` + rest, nil
			}
			return strings.TrimPrefix(path, `\\?\`), nil
		}
		buf = make([]uint16, n)
	}
}
