//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func renameGetFileNoReplace(stagedPath, outputPath string) error {
	stagedName, err := windows.UTF16PtrFromString(stagedPath)
	if err != nil {
		return err
	}
	outputName, err := windows.UTF16PtrFromString(outputPath)
	if err != nil {
		return err
	}
	if err := windows.MoveFile(stagedName, outputName); err != nil {
		return &os.LinkError{Op: "MoveFileW", Old: stagedPath, New: outputPath, Err: err}
	}
	return nil
}
