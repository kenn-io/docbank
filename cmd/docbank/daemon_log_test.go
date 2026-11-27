package main

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/daemonconn"
)

type blockingStderrWriter struct {
	entered     chan struct{}
	enterOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once

	mu  sync.Mutex
	buf bytes.Buffer
}

func newBlockingStderrWriter() *blockingStderrWriter {
	return &blockingStderrWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingStderrWriter) Write(p []byte) (int, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.buf.Write(p)
	return len(p), nil
}

func (w *blockingStderrWriter) Release() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *blockingStderrWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestNonBlockingWriterBuffersDropsAndBoundsClose(t *testing.T) {
	t.Run("flushes writes in order on close", func(t *testing.T) {
		var dst bytes.Buffer
		w := newNonBlockingWriter(&dst, 1024)
		for _, line := range []string{"first\n", "second\n", "third\n"} {
			n, err := w.Write([]byte(line))
			require.NoError(t, err)
			require.Equal(t, len(line), n)
		}
		w.Close(t.Context())
		assert.Equal(t, "first\nsecond\nthird\n", dst.String())
	})

	t.Run("drops whole writes past budget and respects canceled close", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			bw := newBlockingStderrWriter()
			w := newNonBlockingWriter(bw, 16)

			// First write enters dst and blocks; subsequent writes buffer up to 16 bytes
			// and drop whole records beyond the budget without blocking Write.
			_, err := w.Write([]byte("line-1\n"))
			require.NoError(t, err)
			<-bw.entered
			_, err = w.Write([]byte("line-2\n"))
			require.NoError(t, err)
			_, err = w.Write([]byte("line-3-too-big-for-budget\n"))
			require.NoError(t, err)

			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			w.Close(canceled)

			bw.Release()
			synctest.Wait()
			assert.Equal(t, "line-1\nline-2\n", bw.String())
		})
	})
}

func TestDaemonDiscoverySucceedsWhenStderrBlocked(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)

	bw := newBlockingStderrWriter()
	previous := serveStderr
	serveStderr = bw
	t.Cleanup(func() { serveStderr = previous })

	stop := startServe(t)
	t.Cleanup(bw.Release)
	<-bw.entered

	// CLI discovery uses the standard 2-second probe timeout for both /api/ping
	// and /api/daemon/challenge and must succeed while stderr is completely blocked.
	_, info, ok, err := daemonconn.Find(t.Context(), vault)
	require.NoError(t, err)
	require.True(t, ok, "CLI discovery must find the ready daemon even when stderr is blocked")
	assert.True(t, info.OK)

	out, err := runCLI(t, "ls", "/")
	require.NoError(t, err)
	assert.Contains(t, out, "SELECTOR")

	bw.Release()
	stop()

	logged := bw.String()
	assert.Contains(t, logged, `"msg":"docbank daemon listening"`)
	assert.Contains(t, logged, `"msg":"docbank daemon shutting down"`)
}
