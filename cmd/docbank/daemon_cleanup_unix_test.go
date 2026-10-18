//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestServeConfigDiagnosticExcludesBootstrapCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	require.NoError(t, os.Mkdir(root, 0o700))
	t.Setenv("DOCBANK_HOME", root)
	configPath := filepath.Join(root, "config.toml")
	require.NoError(t, syscall.Mkfifo(configPath, 0o600))
	stderr, writer, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = previous; _ = writer.Close(); _ = stderr.Close() })
	done := make(chan error, 1)
	go func() { done <- runServe(t.Context()) }()
	// Opening the writer waits until startup has removed the old bootstrap
	// and opened the config. Replace its root so deferred cleanup fails.
	config, err := os.OpenFile(configPath, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = config.Close() })
	require.NoError(t, os.Rename(root, root+"-moved"))
	require.NoError(t, os.WriteFile(root, nil, 0o600))
	_, err = config.WriteString("[credential_bindings.\"bad\\nname\"]\nenvironment_variable = 'TOKEN'\n")
	require.NoError(t, err)
	require.NoError(t, config.Close())
	err = <-done
	require.ErrorIs(t, err, daemonconn.ErrDaemonConfigInvalid)
	require.Contains(t, err.Error(), "bad\nname")
	require.NotContains(t, err.Error(), "removing web bootstrap")
	require.NoError(t, writer.Close())
	output, err := io.ReadAll(stderr)
	require.NoError(t, err)
	require.Contains(t, string(output), "removing web bootstrap")
}
