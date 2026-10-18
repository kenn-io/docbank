//go:build !windows

package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestKataCredentialFIFOIsRejectedBeforeOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "credential-pipe")
	require.NoError(t, unix.Mkfifo(path, 0600))
	result := make(chan error, 1)
	go func() {
		_, err := (KataConfig{Enabled: true, Endpoint: "http://127.0.0.1:7777", APIKeyFile: path}).Resolve(root)
		result <- err
	}()
	select {
	case err := <-result:
		require.ErrorContains(t, err, "regular file")
	case <-time.After(2 * time.Second):
		// Release the blocked reader so a failed regression leaves no goroutine behind.
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0600)
		if err == nil {
			require.NoError(t, unix.Close(fd))
		}
		select {
		case <-result:
		case <-time.After(2 * time.Second):
			t.Error("credential reader did not stop")
		}
		t.Fatal("credential validation blocked while opening a named pipe")
	}
}
