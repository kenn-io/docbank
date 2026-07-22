//go:build !darwin && !linux && !windows

package main

import (
	"errors"
	"os"
)

func renameGetFileNoReplace(stagedPath, outputPath string) error {
	return &os.LinkError{
		Op: "rename-noreplace", Old: stagedPath, New: outputPath, Err: errors.ErrUnsupported,
	}
}
