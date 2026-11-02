package daemon_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func TestListenUnixRemovesStaleSocketAndBinds(t *testing.T) {
	socketPath := staleUnixSocket(t)
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(t.Context(), ep)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(t, err)
	_ = conn.Close()
}

func TestListenUnixRejectsNonSocketPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := unixSocketPath(t)
	require.NoError(os.WriteFile(socketPath, []byte("not a socket"), 0o600))
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(t.Context(), ep)
	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "refusing to remove non-socket path")

	body, readErr := os.ReadFile(socketPath)
	require.NoError(readErr)
	assert.Equal("not a socket", string(body))
}

func TestListenUnixRejectsLiveSocket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := unixSocketPath(t)
	live, err := (&net.ListenConfig{}).Listen(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	t.Cleanup(func() { _ = live.Close() })
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(t.Context(), ep)
	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "daemon already listening")

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixSerializesConcurrentStaleSocketStartup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	lockPath := filepath.Join(filepath.Dir(socketPath), "daemon.lock")
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	opt := daemon.WithListenLockPath(lockPath)

	const starters = 16
	start := make(chan struct{})
	results := make(chan listenResult, starters)
	for range starters {
		go func() {
			<-start
			listener, err := daemon.Listen(t.Context(), ep, opt)
			results <- listenResult{listener: listener, err: err}
		}()
	}
	close(start)

	var winner net.Listener
	var errors []error
	for range starters {
		result := <-results
		if result.err == nil {
			require.Nil(winner, "only one daemon start should bind the socket")
			winner = result.listener
			continue
		}
		errors = append(errors, result.err)
	}
	require.NotNil(winner)
	t.Cleanup(func() { _ = winner.Close() })
	require.Len(errors, starters-1)
	for _, err := range errors {
		assert.Contains(err.Error(), "daemon already listening")
	}

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixProbesAfterAcquiringLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	lockPath := filepath.Join(filepath.Dir(socketPath), "daemon.lock")
	heldLock := flock.New(lockPath)
	require.NoError(heldLock.Lock())
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = heldLock.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	resultCh := make(chan listenResult, 1)
	go func() {
		listener, err := daemon.Listen(ctx, ep, daemon.WithListenLockPath(lockPath))
		resultCh <- listenResult{listener: listener, err: err}
	}()

	require.NoError(os.Remove(socketPath))
	live, err := (&net.ListenConfig{}).Listen(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	t.Cleanup(func() { _ = live.Close() })

	require.NoError(heldLock.Unlock())
	locked = false
	result := <-resultCh
	require.Error(result.err)
	assert.Nil(result.listener)
	assert.Contains(result.err.Error(), "daemon already listening")

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	_ = conn.Close()
}

type listenResult struct {
	listener net.Listener
	err      error
}

// staleUnixSocket leaves a socket file with no listener, as a daemon that
// exits without closing its listener does.
func staleUnixSocket(t *testing.T) string {
	t.Helper()
	socketPath := unixSocketPath(t)
	listener, err := net.ListenUnix(daemon.NetworkUnix, &net.UnixAddr{Name: socketPath, Net: daemon.NetworkUnix})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())
	_, err = os.Lstat(socketPath)
	require.NoError(t, err, "closed listener did not leave a socket path")
	return socketPath
}

func unixSocketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths must stay short, so Unix tests need a fixed short temp
	// root; the Windows temp dir is short enough.
	root := "/tmp"
	if runtime.GOOS == "windows" {
		root = ""
	}
	dir, err := os.MkdirTemp(root, "kitd") //nolint:usetesting // see the socket path length note above
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Listen requires a private socket dir; a Windows temp dir inherits a
	// broader DACL until restricted.
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	return filepath.Join(dir, "d.sock")
}
