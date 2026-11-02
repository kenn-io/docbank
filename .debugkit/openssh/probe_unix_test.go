//go:build unix

package openssh

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listenUnixSocket(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	directory, err := os.MkdirTemp("", "kit-ssh-") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	path := filepath.Join(directory, "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	return listener, path
}

func TestProbeControlMasterClassifiesAbsentPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	directory, err := os.MkdirTemp("", "kit-ssh-") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.RemoveAll(directory)) })
	manager, err := NewPersistentManager(directory, PersistentConfig{MaximumControlPathBytes: 1_000})
	require.NoError(err)

	state, err := manager.probeControlMaster(t.Context(), filepath.Join(directory, "absent.sock"), testTarget("wes@studio"))

	require.NoError(err)
	assert.Equal(probeAbsent, state)
}

func TestProbeControlMasterClassifiesStaleSocket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	listener, path := listenUnixSocket(t)
	listener.SetUnlinkOnClose(false)
	require.NoError(listener.Close())
	manager, err := NewPersistentManager(filepath.Dir(path), PersistentConfig{MaximumControlPathBytes: 1_000})
	require.NoError(err)

	state, err := manager.probeControlMaster(t.Context(), path, testTarget("wes@studio"))

	require.NoError(err)
	assert.Equal(probeStale, state)
}

func TestProbeControlMasterClassifiesAliveMux(t *testing.T) {
	listener, path := listenUnixSocket(t)
	defer listener.Close()
	manager, err := NewPersistentManager(filepath.Dir(path), PersistentConfig{
		RunSSH:                  func(context.Context, []string) (int, error) { return 0, nil },
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(t, err)

	state, err := manager.probeControlMaster(t.Context(), path, testTarget("wes@studio"))

	require.NoError(t, err)
	assert.Equal(t, probeAlive, state)
}

func TestProbeControlMasterClassifiesOccupiedListener(t *testing.T) {
	listener, path := listenUnixSocket(t)
	defer listener.Close()
	manager, err := NewPersistentManager(filepath.Dir(path), PersistentConfig{
		RunSSH: func(context.Context, []string) (int, error) {
			return 255, errors.New("invalid mux greeting")
		},
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(t, err)

	_, err = manager.probeControlMaster(t.Context(), path, testTarget("wes@studio"))

	require.ErrorIs(t, err, ErrControlPathOccupied)
	assert.FileExists(t, path)
}

func TestProbeControlMasterPreservesIndeterminateListener(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	listener, path := listenUnixSocket(t)
	defer listener.Close()
	sentinel := errors.New("ssh failed to start")
	manager, err := NewPersistentManager(filepath.Dir(path), PersistentConfig{
		RunSSH:                  func(context.Context, []string) (int, error) { return -1, sentinel },
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(err)

	_, err = manager.probeControlMaster(t.Context(), path, testTarget("wes@studio"))

	require.ErrorIs(err, ErrProbeIndeterminate)
	require.ErrorIs(err, sentinel)
	assert.FileExists(path)
}

func TestProbeControlMasterPreservesCanceledContext(t *testing.T) {
	require := require.New(t)
	listener, path := listenUnixSocket(t)
	defer listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	sentinel := errors.New("ssh process canceled")
	manager, err := NewPersistentManager(filepath.Dir(path), PersistentConfig{
		RunSSH: func(context.Context, []string) (int, error) {
			cancel()
			return -1, sentinel
		},
		MaximumControlPathBytes: 1_000,
	})
	require.NoError(err)

	_, err = manager.probeControlMaster(ctx, path, testTarget("wes@studio"))

	require.ErrorIs(err, ErrProbeIndeterminate)
	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(err, sentinel)
}

func (m *PersistentManager) probeControlMaster(
	ctx context.Context,
	socketPath string,
	target Target,
) (masterProbeState, error) {
	return m.probeControlMasterWithRunner(ctx, socketPath, target, m.config.RunSSH)
}
