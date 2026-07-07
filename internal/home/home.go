// Package home resolves the docbank data directory layout.
package home

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout describes the on-disk data directory rooted at Root.
type Layout struct {
	Root string
}

// Resolve returns the layout rooted at $DOCBANK_HOME, defaulting to
// ~/.docbank when unset or empty.
func Resolve() (Layout, error) {
	if root := os.Getenv("DOCBANK_HOME"); root != "" {
		return Layout{Root: root}, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolving home directory: %w", err)
	}
	return Layout{Root: filepath.Join(userHome, ".docbank")}, nil
}

func (l Layout) DBPath() string     { return filepath.Join(l.Root, "docbank.db") }
func (l Layout) BlobsDir() string   { return filepath.Join(l.Root, "blobs") }
func (l Layout) BlobTmpDir() string { return filepath.Join(l.Root, "blobs", "tmp") }
func (l Layout) LogsDir() string    { return filepath.Join(l.Root, "logs") }

// Ensure creates the directory layout if missing.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.BlobsDir(), l.BlobTmpDir(), l.LogsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}
