package main

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestBrowserRestartKeepsDaemonAndEndsSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	available := webAssetsAvailable
	webAssetsAvailable = func() bool { return true }
	t.Cleanup(func() { webAssetsAvailable = available })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := make(chan serveReady, 2)
	done := make(chan error, 1)
	go func() { done <- serveDaemon(ctx, func(r serveReady) { runs <- r }) }()
	waitRun := func() {
		t.Helper()
		select {
		case <-runs:
		case err := <-done:
			require.FailNow(t, "daemon exited", "%v", err)
		case <-time.After(daemonStartTimeout):
			require.FailNow(t, "daemon run never became ready")
		}
	}
	waitRun()
	before := waitForDaemon(t, dir)

	issue, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+before.Address+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	issue.Header.Set("X-Api-Key", before.Metadata["api_key"])
	response, err := http.DefaultClient.Do(issue)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var session struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	require.NoError(t, json.UnmarshalRead(response.Body, &session))
	require.NoError(t, response.Body.Close())

	// The web origin's .localhost name need not resolve on the test host.
	web := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, before.Metadata["web_address"])
	}}}
	defer web.CloseIdleConnections()
	browser := func(method, path string) int {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, session.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set(api.WebSessionHeader, session.Token)
		request.Header.Set("Origin", session.URL[:len(session.URL)-1])
		response, err := web.Do(request)
		require.NoError(t, err)
		_ = response.Body.Close()
		return response.StatusCode
	}
	require.Equal(t, http.StatusAccepted, browser(http.MethodPost, "api/v1/processing/local/restart"))
	waitRun()

	after := waitForDaemon(t, dir)
	assert.Equal(t, before, after, "the runtime record and its listeners survive the restart")
	assert.Equal(t, http.StatusUnauthorized, browser(http.MethodGet, "api/v1/processing/local/status"), "the next run starts without browser sessions")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(daemonShutdownTimeout):
		require.FailNow(t, "daemon did not shut down")
	}
	_, _, found, err := daemonconn.Find(t.Context(), dir)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestSharedListenerHandsConnectionsToTheNextView(t *testing.T) {
	bound, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	shared := shareListener(bound)
	defer shared.close()
	first := shared.view()
	require.NoError(t, first.Close())
	_, err = first.Accept()
	require.ErrorIs(t, err, net.ErrClosed)

	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", bound.Addr().String())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	conn, err := shared.view().Accept()
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestShutdownOverridesALaterRestart(t *testing.T) {
	var p daemonProcess
	p.shutdown.Store(true)
	p.restart.Store(true)
	assert.False(t, p.restartRequested())
}
