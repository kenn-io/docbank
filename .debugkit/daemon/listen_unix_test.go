//go:build !windows

package daemon_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestListenUnixRejectsUnsafeLockDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	base, err := os.MkdirTemp("/tmp", "kitd-lock") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	require.NoError(os.MkdirAll(target, 0o700))
	require.NoError(os.Symlink(target, link))

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithListenLockPath(filepath.Join(link, "daemon.lock")))

	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "prepare daemon lock dir")
	assert.Contains(err.Error(), "symlink")
	_, statErr := os.Lstat(socketPath)
	require.NoError(statErr, "stale socket should not be touched when lock dir is unsafe")
}

func TestListenUnixRejectsRelativeLockPath(t *testing.T) {
	assert := assert.New(t)

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: unixSocketPath(t)}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithListenLockPath("daemon.lock"))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "daemon lock path")
	assert.Contains(err.Error(), "must be absolute")
}

func TestListenUnixRejectsRelativeSocketPath(t *testing.T) {
	assert := assert.New(t)

	lockPath := filepath.Join(t.TempDir(), "daemon.lock")
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: "daemon.sock"}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithListenLockPath(lockPath))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "unix socket path")
	assert.Contains(err.Error(), "must be absolute")
}

func TestListenUnixRejectsSharedSocketDirectoryEvenWithStoreLock(t *testing.T) {
	assert := assert.New(t)

	socketPath := filepath.Join("/tmp", "kitd-shared-socket.sock")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithRuntimeStore(daemon.RuntimeStore{Dir: t.TempDir()}))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "validate unix socket dir")
	_, statErr := os.Lstat(socketPath)
	assert.True(os.IsNotExist(statErr), "socket in shared dir should not be created: %v", statErr)
}
