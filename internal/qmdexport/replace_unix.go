//go:build !windows

package qmdexport

import "os"

func atomicReplace(source, destination string) error { return os.Rename(source, destination) }
