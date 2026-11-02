//go:build unix

package openssh

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectControlSocketRejectsNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	_, err := inspectControlSocket(t.Context(), path)

	var securityErr *ControlPathSecurityError
	require.ErrorAs(t, err, &securityErr)
	assert.Equal(t, path, securityErr.Path)
}

func TestInspectControlSocketDistinguishesListeningAndStale(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	directory, err := os.MkdirTemp("", "kit-ssh-") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.RemoveAll(directory)) })

	listeningPath := filepath.Join(directory, "listening.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: listeningPath, Net: "unix"})
	require.NoError(err)
	defer listener.Close()

	state, err := inspectControlSocket(t.Context(), listeningPath)
	require.NoError(err)
	assert.Equal(socketListening, state)

	stalePath := filepath.Join(directory, "stale.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: stalePath, Net: "unix"})
	require.NoError(err)
	stale.SetUnlinkOnClose(false)
	require.NoError(stale.Close())

	state, err = inspectControlSocket(t.Context(), stalePath)
	require.NoError(err)
	assert.Equal(socketStale, state)
}
