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

// Ensure creates the directory layout if missing and enforces the privacy
// the design documents rather than assuming it: every layout directory is
// 0700 and the database file 0600, tightening pre-existing lax modes.
// MkdirAll alone would trust an existing world-readable root, and SQLite
// creates a missing database with umask-derived permissions — pre-creating
// it at 0600 also covers the WAL and SHM siblings, which SQLite gives the
// database file's mode.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.BlobsDir(), l.BlobTmpDir(), l.LogsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		if err := enforceMode(dir, 0o700); err != nil {
			return err
		}
	}
	db, err := os.OpenFile(l.DBPath(), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", l.DBPath(), err)
	}
	_ = db.Close()
	return enforceMode(l.DBPath(), 0o600)
}

// enforceMode chmods path to mode unless it already matches. On platforms
// without POSIX permissions (Windows) Chmod is a near no-op and the vault
// is unsupported anyway (see lock_stub.go).
func enforceMode(path string, mode os.FileMode) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking permissions of %s: %w", path, err)
	}
	if fi.Mode().Perm() == mode {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("tightening permissions of %s: %w", path, err)
	}
	return nil
}
