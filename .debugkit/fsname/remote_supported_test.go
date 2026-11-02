//go:build linux || darwin || dragonfly || freebsd || openbsd || windows

package fsname

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteLocalTempDir(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	remote, err := Remote(dir)
	require.NoError(err)
	assert.False(t, remote, "Remote(%q)", dir)

	path := filepath.Join(dir, "file")
	require.NoError(os.WriteFile(path, []byte("x"), 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = f.Close() })

	remote, err = RemoteFile(f)
	require.NoError(err)
	assert.False(t, remote, "RemoteFile(%q)", path)
}

func TestRemoteMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")

	remote, err := Remote(path)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.False(t, remote)
}
