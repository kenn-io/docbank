package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"
)

var syncGetDestinationDir = pack.SyncDir

func publishGetFile(stagedPath, outputPath string, overwrite bool) error {
	if overwrite {
		if err := os.Rename(stagedPath, outputPath); err != nil {
			return err
		}
	} else {
		linkErr := os.Link(stagedPath, outputPath)
		if linkErr != nil {
			if renameErr := renameGetFileNoReplace(stagedPath, outputPath); renameErr != nil {
				return errors.Join(
					fmt.Errorf("hard-link publication: %w", linkErr),
					fmt.Errorf("no-replace rename publication: %w", renameErr),
				)
			}
		}
	}
	if err := syncGetDestinationDir(filepath.Dir(outputPath)); err != nil {
		return fmt.Errorf("file published at %s but destination directory sync failed: %w",
			outputPath, err)
	}
	return nil
}
