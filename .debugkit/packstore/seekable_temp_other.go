//go:build !unix && !windows

package packstore

import (
	"fmt"
	"os"
	"runtime"
)

// Platforms without unlink-while-open or handle-bound deletion fail closed;
// cleanup must never fall back to deleting a pathname after closing its inode.
func createSeekableLooseTempPlatform() (*os.File, error) {
	return nil, fmt.Errorf("packstore: seekable compressed loose temporary files are unsupported on %s", runtime.GOOS)
}
