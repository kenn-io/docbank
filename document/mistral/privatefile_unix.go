//go:build !windows

package mistral

import (
	"errors"
	"os"

	"go.kenn.io/kit/safefileio"
)

func validatePrivateDirectory(directory string) error {
	if err := safefileio.ValidatePrivateDir(directory); err != nil {
		return errors.New("mistral OCR spool directory must already exist and be private")
	}
	return nil
}

func secureCreatedFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := safefileio.ValidateCurrentUserFile(file); err != nil {
		return errors.New("mistral OCR spool file must be owned by the current user")
	}
	return nil
}

func openPrivateFile(name string) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("mistral OCR spool must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("mistral OCR spool permissions must be private")
	}
	file, err := safefileio.OpenCurrentUserFile(name)
	if err != nil {
		return nil, errors.New("mistral OCR spool must be a non-symlink file owned by the current user")
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("mistral OCR spool changed while opening")
	}
	return file, nil
}
