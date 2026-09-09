//go:build !windows

package qmdexport

import (
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kit/safefileio"
)

func restrictNewFile(string) error { return nil }

func openPrivateFile(path string) (*os.File, error) {
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("open private qmd export file: %w", err)
	}
	info, err := file.Stat()
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		err = errors.New("qmd export file permits access by other users")
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func renamePublished(source, destination string) error { return os.Rename(source, destination) }
