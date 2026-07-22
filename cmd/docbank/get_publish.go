package main

import (
	"errors"
	"fmt"
	"os"
)

func publishGetFile(stagedPath, outputPath string, overwrite bool) error {
	if overwrite {
		return os.Rename(stagedPath, outputPath)
	}
	linkErr := os.Link(stagedPath, outputPath)
	if linkErr == nil {
		return nil
	}
	if renameErr := renameGetFileNoReplace(stagedPath, outputPath); renameErr != nil {
		return errors.Join(
			fmt.Errorf("hard-link publication: %w", linkErr),
			fmt.Errorf("no-replace rename publication: %w", renameErr),
		)
	}
	return nil
}
