//go:build unix

package fslink_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
)

func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	// A blocking open would wait for a writer that never arrives.
	_, err := fslink.OpenRegular(path)
	assert.ErrorContains(t, err, "not a regular file")
}

func TestOpenRootRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	// A blocking open would wait for a writer that never arrives.
	root, err := fslink.OpenRoot(path)
	assert.Nil(t, root)
	assert.ErrorIs(t, err, syscall.ENOTDIR)
}

// ".." after a link applies to the link's destination, as the kernel
// resolves it, not lexically.
func TestOpenRootResolvesDotDotAfterLink(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a", "c"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("a", "b"), filepath.Join(dir, "link")))

	root, err := fslink.OpenRoot(filepath.Join(dir, "link") + "/../c")
	require.NoError(t, err)
	require.NoError(t, root.Close())
}

func TestLinkDirCreatesSymlink(t *testing.T) {
	dir := newTree(t)
	kind, err := fslink.LinkDir(filepath.Join(dir, "sub"), filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.Equal(t, fslink.Symlink, kind)
}

func TestCreateJunctionUnsupported(t *testing.T) {
	dir := newTree(t)
	err := fslink.CreateJunction(filepath.Join(dir, "sub"), filepath.Join(dir, "link"))
	require.ErrorIs(t, err, errors.ErrUnsupported)
	assert.NoFileExists(t, filepath.Join(dir, "link"))
}
