package safefileio

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// createPrivateTempAttempts bounds CreatePrivateTemp retries, matching
// os.CreateTemp.
const createPrivateTempAttempts = 10000

// CreatePrivateTemp creates a new private file in dir with CreatePrivateFile.
// The file name is built from pattern like os.CreateTemp: the last "*" is
// replaced by a random string, or the random string is appended when pattern
// has no "*". An empty dir means os.TempDir. Patterns containing a path
// separator are rejected.
func CreatePrivateTemp(dir, pattern string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	for i := range len(pattern) {
		if os.IsPathSeparator(pattern[i]) {
			return nil, &fs.PathError{Op: "createtemp", Path: pattern, Err: errors.New("pattern contains path separator")}
		}
	}
	prefix, suffix, _ := strings.CutLast(pattern, "*")
	for range createPrivateTempAttempts {
		name := prefix + strconv.FormatUint(uint64(rand.Uint32()), 10) + suffix
		file, err := CreatePrivateFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, prefix+"*"+suffix), Err: fs.ErrExist}
}
