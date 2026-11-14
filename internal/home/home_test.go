package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveUsesEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	l, err := Resolve()
	require.NoError(t, err)
	canonical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, canonical, l.Root)
	assert.Equal(t, filepath.Join(canonical, "docbank.db"), l.DBPath())
	assert.Equal(t, filepath.Join(canonical, "blobs"), l.BlobsDir())
	assert.Equal(t, filepath.Join(canonical, "blobs", "tmp"), l.BlobTmpDir())
	assert.Equal(t, filepath.Join(canonical, "logs"), l.LogsDir())
}

func TestResolveDefaultsToHomeDir(t *testing.T) {
	t.Setenv("DOCBANK_HOME", "")

	l, err := Resolve()
	require.NoError(t, err)
	userHome, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(userHome, ".docbank"), l.Root)
}

func TestEnsureCreatesLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	l := Layout{Root: dir}
	require.NoError(t, l.Ensure())

	for _, p := range []string{dir, l.BlobsDir(), l.BlobTmpDir(), l.EmailPDFSpoolDir(), l.LogsDir()} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	}
	// Idempotent.
	require.NoError(t, l.Ensure())
}

func TestDirectoryContainment(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "vault")
	child := filepath.Join(root, "child")
	require.NoError(t, os.MkdirAll(child, 0o700))
	layout := Layout{Root: root}

	inside, err := layout.ContainsDirectory(child)
	require.NoError(t, err)
	assert.True(t, inside)
	inside, err = layout.ContainsDirectory(parent)
	require.NoError(t, err)
	assert.False(t, inside)
	_, err = layout.ContainsDirectory(filepath.Join(root, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)

	inside, err = PathWithin(root, filepath.Join(root, "missing", "child"))
	require.NoError(t, err)
	assert.True(t, inside)
	inside, err = PathWithin(root, filepath.Join(parent, "missing", "child"))
	require.NoError(t, err)
	assert.False(t, inside)
	inside, err = PathWithin(root, root)
	require.NoError(t, err)
	assert.True(t, inside)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, path := range []string{alias, filepath.Join(alias, "missing")} {
		inside, err = PathWithin(root, path)
		require.NoError(t, err)
		assert.True(t, inside)
	}
}
