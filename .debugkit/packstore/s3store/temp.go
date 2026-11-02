package s3store

import (
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/safefileio"
)

func createPrivateTemp(pattern string) (*os.File, error) {
	return createPrivateTempIn(os.TempDir(), pattern)
}

func createPrivateTempIn(base, pattern string) (*os.File, error) {
	userID, err := safefileio.CurrentUserID()
	if err != nil {
		return nil, fmt.Errorf("s3store: identify private staging user: %w", err)
	}
	dir := filepath.Join(base, "kit-s3-"+userID)
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("s3store: prepare private staging directory: %w", err)
	}
	if err := safefileio.ValidatePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("s3store: validate private staging directory: %w", err)
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("s3store: create private staging file: %w", err)
	}
	return file, nil
}
