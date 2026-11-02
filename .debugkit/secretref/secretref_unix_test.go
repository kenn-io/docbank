//go:build unix

package secretref_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/secretref"
)

func TestRefFileMustBePrivateAndNotALink(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.key")
	require.NoError(t, os.WriteFile(shared, []byte("secret"), 0o600))
	require.NoError(t, os.Chmod(shared, 0o644))
	private := filepath.Join(dir, "private.key")
	writePrivateFile(t, private, "secret")
	link := filepath.Join(dir, "link.key")
	require.NoError(t, os.Symlink(private, link))

	for path, reason := range map[string]string{
		shared: "file must be private to its owner (mode 0600)",
		link:   "file must be a regular file owned by the current user",
	} {
		_, err := secretref.Ref{File: path}.Resolve()
		require.ErrorContains(t, err, reason)
	}
}

func TestRefFileRefusesAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pipe")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	done := make(chan error, 1)
	go func() {
		_, err := secretref.Ref{File: path}.Resolve()
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "file must be a regular file owned by the current user")
	case <-time.After(5 * time.Second):
		// A blocked open waits for a FIFO writer, kernel state that synctest
		// cannot observe, so this wait is wall-clock. Open a writer to
		// release the goroutine before failing.
		writer, err := os.OpenFile(path, os.O_RDWR, 0o600)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		<-done
		require.FailNow(t, "resolving a FIFO key file must not block")
	}
}
