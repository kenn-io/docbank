//go:build unix

package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureTightensLaxPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	require.NoError(t, os.MkdirAll(root, 0o755))
	// Chmod, not creation modes: the process umask would silently tighten
	// these and let the test pass against code that enforces nothing.
	require.NoError(t, os.Chmod(root, 0o755))
	l := Layout{Root: root}
	db := l.DBPath()
	require.NoError(t, os.WriteFile(db, nil, 0o644))
	require.NoError(t, os.Chmod(db, 0o644))

	// A pre-existing world-readable root (and umask-derived db mode) must
	// be tightened, not trusted: the documented 0700 boundary is enforced
	// on every open, and SQLite's WAL/SHM inherit the db file's mode.
	require.NoError(t, l.Ensure())

	for _, p := range []string{root, l.BlobsDir(), l.BlobTmpDir(), l.LogsDir()} {
		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), p)
	}
	fi, err := os.Stat(db)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestEnsureCreatesPrivateDB(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "vault")}
	require.NoError(t, l.Ensure())
	fi, err := os.Stat(l.DBPath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestResolveCanonicalizesSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "vault")
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.MkdirAll(realRoot, 0o700))
	require.NoError(t, os.Symlink(realRoot, alias))
	t.Setenv("DOCBANK_HOME", alias)

	layout, err := Resolve()
	require.NoError(t, err)
	canonical, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	require.Equal(t, canonical, layout.Root)
}

func TestCanonicalRootResolvesExistingSymlinkAncestor(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Mkdir(realParent, 0o700))
	require.NoError(t, os.Symlink(realParent, alias))

	root, err := CanonicalRoot(filepath.Join(alias, "missing", "vault"))
	require.NoError(t, err)
	canonicalParent, err := filepath.EvalSymlinks(realParent)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(canonicalParent, "missing", "vault"), root)
}

func TestCanonicalRootResolvesSymlinkBeforeParentTraversal(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	realChild := filepath.Join(realParent, "child")
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.MkdirAll(realChild, 0o700))
	require.NoError(t, os.Symlink(realChild, alias))

	root, err := CanonicalRoot(alias + string(os.PathSeparator) + ".." +
		string(os.PathSeparator) + "vault")
	require.NoError(t, err)
	canonicalParent, err := filepath.EvalSymlinks(realParent)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(canonicalParent, "vault"), root)
}
