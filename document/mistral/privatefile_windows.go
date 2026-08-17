//go:build windows

package mistral

import (
	"errors"
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

func validatePrivateDirectory(directory string) error {
	if err := safefileio.ValidatePrivateDir(directory); err != nil {
		return errors.New("mistral OCR spool directory must already exist and have a restricted DACL")
	}
	return nil
}

func secureCreatedFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := winsecurity.RestrictCurrentUserFile(file.Name()); err != nil {
		return err
	}
	verified, err := winsecurity.OpenRestrictedCurrentUserFile(file.Name())
	if err != nil {
		return err
	}
	defer func() { _ = verified.Close() }()
	verifiedInfo, err := verified.Stat()
	if err != nil || !os.SameFile(info, verifiedInfo) {
		return errors.New("mistral OCR spool changed while securing its DACL")
	}
	return nil
}

func openPrivateFile(name string) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	file, err := winsecurity.OpenRestrictedCurrentUserFile(name)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("mistral OCR spool changed while opening")
	}
	return file, nil
}
