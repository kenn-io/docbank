package main

import (
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"
)

var syncGetDestinationDir = pack.SyncDir

func publishGetFile(stagedPath, outputPath string, overwrite bool) error {
	if overwrite {
		if err := os.Rename(stagedPath, outputPath); err != nil {
			return err
		}
	} else if err := atomicfile.PublishNoReplace(stagedPath, outputPath); err != nil {
		return fmt.Errorf("publish without replacing: %w", err)
	}
	if err := syncGetDestinationDir(filepath.Dir(outputPath)); err != nil {
		return fmt.Errorf("file published at %s but destination directory sync failed: %w",
			outputPath, err)
	}
	return nil
}
