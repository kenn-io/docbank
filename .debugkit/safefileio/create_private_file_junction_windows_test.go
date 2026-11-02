//go:build windows

package safefileio_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

// A junction needs no privilege, so unlike the symlink case this never skips.
func TestCreatePrivateFileRejectsExistingJunction(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(os.Mkdir(target, 0o700))
	require.NoError(os.WriteFile(filepath.Join(target, "inner.txt"), []byte("inner"), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(fslink.CreateJunction(target, link))

	file, err := safefileio.CreatePrivateFile(link)

	require.ErrorIs(err, fs.ErrExist)
	require.Nil(file)
	kind, err := fslink.Classify(link)
	require.NoError(err)
	assert.Equal(t, fslink.Junction, kind)
	entries, err := os.ReadDir(target)
	require.NoError(err)
	require.Len(entries, 1)
	assert.Equal(t, "inner.txt", entries[0].Name())
	data, err := os.ReadFile(filepath.Join(target, "inner.txt"))
	require.NoError(err)
	assert.Equal(t, "inner", string(data))
}
