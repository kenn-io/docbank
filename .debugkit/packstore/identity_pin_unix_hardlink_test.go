//go:build unix && !linux

package packstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHardlinkIdentityPinCloseCleansOwnedPathWithoutCapturedIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "exclusive-pin")
	require.NoError(os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "pinned")
	require.NoError(os.WriteFile(path, []byte("owned pin"), 0o600))
	pin := &hardlinkIdentityPin{path: path, dir: dir}

	var closeErr error
	require.NotPanics(func() {
		closeErr = pin.Close()
	})
	require.NoError(closeErr)
	assert.NoFileExists(path)
	assert.NoDirExists(dir)
}

func TestHardlinkIdentityPinStatRejectsPrivatePathReplacement(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "exclusive-pin")
	require.NoError(os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "pinned")
	require.NoError(os.WriteFile(path, []byte("verified pin"), 0o600))
	identity, err := os.Lstat(path)
	require.NoError(err)
	require.NoError(os.Link(path, filepath.Join(dir, "held")), "keep the verified inode allocated")
	pin := &hardlinkIdentityPin{path: path, dir: dir, identity: identity}
	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(path, []byte("replacement"), 0o600))

	_, err = pin.Stat()

	require.ErrorIs(err, errIdentityChanged)
}
