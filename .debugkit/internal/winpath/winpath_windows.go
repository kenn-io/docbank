package winpath

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// shortLimit is the length below which Win32 accepts a path without the
// \\?\ prefix. It matches the os package: MAX_PATH less the 12 characters a
// directory path must leave for an 8.3 file name.
const shortLimit = 248

// Long returns path in a form Win32 file APIs accept at any length, as the
// os package does: a path shorter than shortLimit (counting the working
// directory for a relative path) or already in the \\?\ or \??\ form is
// returned unchanged; a longer one is made absolute and clean with
// GetFullPathName and given the \\?\ or \\?\UNC\ prefix. A device path
// (\\.\) is returned unchanged.
func Long(path string) string {
	if isExtended(path) {
		return path
	}
	n := len(path)
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			n += len(wd) + 1
		}
	}
	if n < shortLimit {
		return path
	}
	full, err := windows.FullPath(path)
	if err != nil {
		return path
	}
	switch {
	case len(full) >= 4 && isSep(full[0]) && isSep(full[1]) && full[2] == '.' && isSep(full[3]):
		return path
	case len(full) >= 2 && isSep(full[0]) && isSep(full[1]):
		return `\\?\UNC\` + full[2:]
	default:
		return `\\?\` + full
	}
}

// UTF16Ptr is windows.UTF16PtrFromString applied to Long(path).
func UTF16Ptr(path string) (*uint16, error) {
	return windows.UTF16PtrFromString(Long(path))
}

func isExtended(path string) bool {
	if strings.HasPrefix(path, `\??\`) {
		return true
	}
	return len(path) >= 4 && isSep(path[0]) && isSep(path[1]) && path[2] == '?' && isSep(path[3])
}

func isSep(c byte) bool { return c == '\\' || c == '/' }
