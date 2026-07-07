//go:build !unix

package ingest

import "os"

// openNoFollow falls back to a plain open where O_NOFOLLOW is unavailable;
// docbank refuses to open a vault on non-Unix platforms anyway (see
// internal/home), so this only keeps the package compiling there.
func openNoFollow(src string) (*os.File, error) { return os.Open(src) }
