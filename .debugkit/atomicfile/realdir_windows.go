//go:build windows

package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

// resolveLinkDest returns the path named by a link in linkDir whose
// destination is dest. A relative dest is joined to linkDir's real directory,
// since links in linkDir itself are resolved before the destination is
// applied. Joining applies ".." in dest lexically, which matches Windows:
// Win32 path normalization removes ".." before the kernel resolves any
// reparse point in the remaining path. A destination rooted without a volume
// (`\dir`) names a path on the link's volume.
func resolveLinkDest(linkDir, dest string) (string, error) {
	if filepath.IsAbs(dest) {
		return dest, nil
	}
	dir, err := realDir(linkDir)
	if err != nil {
		return "", err
	}
	if filepath.VolumeName(dest) == "" && dest != "" && os.IsPathSeparator(dest[0]) {
		return filepath.VolumeName(dir) + dest, nil
	}
	return filepath.Join(dir, dest), nil
}

// realDir returns the final path of the directory dir after the system
// resolves every symlink and junction in it, which is the directory a link
// inside dir resolves its relative destination against.
// filepath.EvalSymlinks does not traverse junctions.
func realDir(dir string) (string, error) {
	dir16, err := winpath.UTF16Ptr(dir)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: dir, Err: err}
	}
	handle, err := windows.CreateFile(
		dir16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: dir, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	// Flags 0 is FILE_NAME_NORMALIZED|VOLUME_NAME_DOS: a `\\?\` path.
	n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: dir, Err: err}
	}
	final := windows.UTF16ToString(buf[:n])
	if rest, ok := strings.CutPrefix(final, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(final, `\\?\`), nil
}

// canonicalPath returns path made absolute. Win32 normalization applies ".."
// lexically before reparse points resolve, so cleaning matches the system.
func canonicalPath(path string) (string, error) {
	return filepath.Abs(path)
}
