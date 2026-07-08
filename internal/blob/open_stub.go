//go:build !unix

package blob

import "os"

// OpenNoFollow falls back to a plain open where O_NOFOLLOW is unavailable;
// docbank refuses to open a vault on non-Unix platforms anyway (see
// internal/home), so this only keeps the package compiling there.
func OpenNoFollow(path string) (*os.File, error) { return os.Open(path) }
