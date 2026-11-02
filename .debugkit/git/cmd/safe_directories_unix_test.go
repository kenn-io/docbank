//go:build unix

package gitcmd

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeDirectoryFingerprintRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitconfig")
	err := syscall.Mkfifo(path, 0o600)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("filesystem does not support FIFOs")
	}
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			err = writer.Close()
		}
		done <- err
	}()
	t.Cleanup(func() {
		reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		require.NoError(t, err)
		require.NoError(t, <-done)
		require.NoError(t, reader.Close())
	})
	_, err = readSafeDirectorySnapshot(t.Context(), safeDirectoryTestEnv(t, path), "", "global", safeDirectoryScope{paths: []string{path}})
	require.ErrorContains(t, err, "not a regular file")
}
