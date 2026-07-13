// Package home resolves the docbank data directory layout.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Layout describes the on-disk data directory rooted at Root.
type Layout struct {
	Root string
}

// Resolve returns the layout rooted at $DOCBANK_HOME, defaulting to
// ~/.docbank when unset or empty.
func Resolve() (Layout, error) {
	if root := os.Getenv("DOCBANK_HOME"); root != "" {
		return resolveLayout(root)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolving home directory: %w", err)
	}
	return resolveLayout(filepath.Join(userHome, ".docbank"))
}

func resolveLayout(root string) (Layout, error) {
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return Layout{}, err
	}
	return Layout{Root: canonical}, nil
}

// CanonicalRoot returns an absolute vault path with every existing component
// resolved. Missing final components retain their spelling beneath the
// resolved existing ancestor, so discovery and a subsequently launched daemon
// agree before and after first creation.
func CanonicalRoot(root string) (string, error) {
	target := root
	if !filepath.IsAbs(target) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolving vault root %s: %w", root, err)
		}
		target = cwd + string(os.PathSeparator) + target
	}
	current := strings.TrimRight(target, string(os.PathSeparator))
	if current == "" {
		current = string(os.PathSeparator)
	}
	var missing []string
	for {
		_, err := os.Lstat(current) //nolint:gosec // user-selected vault path is the intended lookup target
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("checking vault root %s: %w", target, err)
		}
		parent, component := rawPathParent(current)
		if parent == current {
			return "", fmt.Errorf("vault root has no existing ancestor: %s", target)
		}
		if component == "." || component == ".." {
			return "", fmt.Errorf(
				"vault root traverses %q after a missing component: %s", component, target)
		}
		missing = append(missing, component)
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", fmt.Errorf("resolving vault root %s: %w", target, err)
	}
	slices.Reverse(missing)
	return filepath.Join(append([]string{resolved}, missing...)...), nil
}

func rawPathParent(path string) (string, string) {
	volume := filepath.VolumeName(path)
	rest := path[len(volume):]
	rest = strings.TrimRight(rest, string(os.PathSeparator))
	index := strings.LastIndex(rest, string(os.PathSeparator))
	if index < 0 {
		return path, ""
	}
	component := rest[index+1:]
	parentRest := strings.TrimRight(rest[:index], string(os.PathSeparator))
	if parentRest == "" {
		parentRest = string(os.PathSeparator)
	}
	return volume + parentRest, component
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
