//go:build !windows

package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestDefaultSocketPathRepairsPublicTempFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tempDir := "/tmp"
	service := fmt.Sprintf("kitdpublic%d", os.Getpid())
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("XDG_RUNTIME_DIR", "")
	parent := filepath.Join(tempDir, fmt.Sprintf("%s-%d", service, os.Getuid()))
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	require.NoError(os.MkdirAll(parent, 0o700))
	require.NoError(os.Chmod(parent, 0o777))

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(socketPath)
	assert.Equal(filepath.Join(parent, "daemon.sock"), socketPath)
	info, err := os.Stat(parent)
	require.NoError(err)
	assert.Zero(info.Mode().Perm() & 0o077)
}

func TestDefaultSocketPathRejectsSymlinkedTempFallback(t *testing.T) {
	tempDir := "/tmp"
	service := fmt.Sprintf("kitdsymlink%d", os.Getpid())
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("XDG_RUNTIME_DIR", "")
	parent := filepath.Join(tempDir, fmt.Sprintf("%s-%d", service, os.Getuid()))
	target := filepath.Join(tempDir, service+"-target")
	t.Cleanup(func() {
		_ = os.Remove(parent)
		_ = os.RemoveAll(target)
	})
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.Symlink(target, parent))

	assert.Empty(t, daemon.DefaultSocketPath(service))
}

func TestDefaultSocketPathCreatesPrivateXDGRuntimeDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	xdg := filepath.Join("/tmp", fmt.Sprintf("kitdxdg%d", os.Getpid()))
	service := "svc"
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	require.NoError(os.MkdirAll(xdg, 0o700))
	require.NoError(os.Chmod(xdg, 0o700))
	t.Setenv("XDG_RUNTIME_DIR", xdg)

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(socketPath)
	parent := filepath.Join(xdg, service)
	assert.Equal(filepath.Join(parent, "daemon.sock"), socketPath)

	info, err := os.Stat(parent)
	require.NoError(err)
	assert.True(info.IsDir())
	assert.Zero(info.Mode().Perm() & 0o077)
}
