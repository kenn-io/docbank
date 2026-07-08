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
	assert.Equal(t, dir, l.Root)
	assert.Equal(t, filepath.Join(dir, "docbank.db"), l.DBPath())
	assert.Equal(t, filepath.Join(dir, "blobs"), l.BlobsDir())
	assert.Equal(t, filepath.Join(dir, "blobs", "tmp"), l.BlobTmpDir())
	assert.Equal(t, filepath.Join(dir, "logs"), l.LogsDir())
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

	for _, p := range []string{dir, l.BlobsDir(), l.BlobTmpDir(), l.LogsDir()} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	}
	// Idempotent.
	require.NoError(t, l.Ensure())
}
