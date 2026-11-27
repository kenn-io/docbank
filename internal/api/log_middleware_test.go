package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	kitdaemon "go.kenn.io/kit/daemon"
)

type blockingLogWriter struct {
	entered     chan struct{}
	enterOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once

	mu  sync.Mutex
	buf bytes.Buffer
}

func newBlockingLogWriter() *blockingLogWriter {
	return &blockingLogWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.buf.Write(p)
	return len(p), nil
}

func (w *blockingLogWriter) Release() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *blockingLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestLogMiddlewareDoesNotBlockResponseFlush(t *testing.T) {
	t.Parallel()
	const shutdownToken = "synthetic-shutdown-token"

	bw := newBlockingLogWriter()
	t.Cleanup(bw.Release)

	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	srv := NewServer(Deps{
		Cfg:           cfg,
		Logger:        slog.New(slog.NewJSONHandler(bw, nil)),
		ShutdownToken: shutdownToken,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		bw.Release()
		ts.Close()
		srv.Close()
	})

	client := ts.Client()

	// First request triggers the log sink and blocks it before any log bytes are written.
	// Its buffered (<2 KiB) response must still flush immediately to the client.
	pingReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+kitPingPath, nil)
	require.NoError(t, err)
	pingResp, err := client.Do(pingReq)
	require.NoError(t, err)
	var ping kitdaemon.PingInfo
	require.NoError(t, json.UnmarshalRead(pingResp.Body, &ping))
	require.NoError(t, pingResp.Body.Close())
	require.Equal(t, http.StatusOK, pingResp.StatusCode)
	assert.True(t, ping.OK)

	<-bw.entered

	// Ownership challenge must also flush its small JSON proof while the log sink stays blocked.
	nonce := make([]byte, config.DaemonAuthNonceBytes)
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	challengeReq, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		ts.URL+config.DaemonChallengePath+"?nonce="+hex.EncodeToString(nonce),
		nil,
	)
	require.NoError(t, err)
	challengeResp, err := client.Do(challengeReq)
	require.NoError(t, err)
	var challenge struct {
		Proof string `json:"proof"`
	}
	require.NoError(t, json.UnmarshalRead(challengeResp.Body, &challenge))
	require.NoError(t, challengeResp.Body.Close())
	require.Equal(t, http.StatusOK, challengeResp.StatusCode)
	assert.True(t, config.VerifyDaemonAuthProof(shutdownToken, nonce, challenge.Proof))

	// Overflowing the bounded request-log queue must keep serving without blocking.
	const overflow = 5
	for range maxPendingRequestLogs + overflow {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+kitPingPath, nil)
		require.NoError(t, reqErr)
		resp, doErr := client.Do(req)
		require.NoError(t, doErr)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}

	bw.Release()
	require.NoError(t, srv.Shutdown(t.Context()))

	logged := bw.String()
	assert.Contains(t, logged, `"path":"/api/ping"`)
	assert.Contains(t, logged, `"path":"/api/daemon/challenge"`)
	assert.Contains(t, logged, `"msg":"request log queue full; dropped request logs"`)
}

func TestRequestLoggerShutdownRespectsCanceledContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		bw := newBlockingLogWriter()
		logger := newRequestLogger(slog.New(slog.NewJSONHandler(bw, nil)))

		logger.enqueue(requestLogEntry{
			method: http.MethodGet,
			path:   kitPingPath,
			remote: "127.0.0.1:1234",
		})
		<-bw.entered

		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		logger.shutdown(canceled)

		// Unblock the drain goroutine before the synctest bubble exits.
		bw.Release()
		synctest.Wait()
		assert.Contains(t, bw.String(), `"path":"/api/ping"`)
	})
}
