//go:build !windows && !unix

package packstore

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
)

func snapshotPathIdentity(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func openNoFollow(path string, _ bool) (*os.File, error) {
	return nil, fmt.Errorf("packstore: unsupported platform %s: race-safe open unavailable for %s", runtime.GOOS, path)
}

func openLooseRepairPin(path string) (*os.File, fs.FileInfo, error) {
	return openLooseFile(path)
}

func validatePlatformFileInfo(fs.FileInfo) error { return nil }
