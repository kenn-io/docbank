package daemon_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestParseEndpointDefaultTCP(t *testing.T) {
	assert := assert.New(t)

	ep, err := daemon.ParseEndpoint("", daemon.ParseEndpointOptions{
		DefaultTCPAddress: "127.0.0.1:7373",
		TCPPolicy:         daemon.RequireLoopback,
	})
	require.NoError(t, err)
	assert.Equal(daemon.NetworkTCP, ep.Network)
	assert.Equal("127.0.0.1:7373", ep.Address)
	assert.Equal("http://127.0.0.1:7373", ep.BaseURL())
	assert.Equal(7373, ep.Port())
}

func TestParseEndpointRejectsPublicTCPWhenPolicyRequiresNonPublic(t *testing.T) {
	_, err := daemon.ParseEndpoint("8.8.8.8:80", daemon.ParseEndpointOptions{
		TCPPolicy: daemon.RequireNonPublic,
	})
	require.Error(t, err)
}

func TestRequireNonPublicAllowsCGNAT(t *testing.T) {
	require.NoError(t, daemon.RequireNonPublic("100.64.0.1:7777"))
}

func TestRequireNonPublicAllowsLocalhost(t *testing.T) {
	require.NoError(t, daemon.RequireNonPublic("localhost:7777"))
}

func TestParseEndpointUnixDefault(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	ep, err := daemon.ParseEndpoint("unix://", daemon.ParseEndpointOptions{
		DefaultUnixPath: socketPath,
	})
	require.NoError(t, err)
	assert.True(t, ep.IsUnix())
	assert.Equal(t, "unix://"+socketPath, ep.ConfigAddress())
}

func TestUnixHTTPClientDialsSocket(t *testing.T) {
	require := require.New(t)

	socketDir, err := os.MkdirTemp("", "kitd") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "daemon.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
	require.NoError(err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.WithoutCancel(t.Context())) })

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	resp, err := doRequest(t, ep.HTTPClient(daemon.HTTPClientOptions{}), http.MethodGet, ep.BaseURL(), "", nil)
	require.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestTCPHTTPClientBypassesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")

	ep := daemon.Endpoint{Network: daemon.NetworkTCP, Address: "10.0.0.1:7373"}
	client := ep.HTTPClient(daemon.HTTPClientOptions{})
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, transport.Proxy)
}

func TestDefaultSocketPathCreatesPrivateTempFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	service := fmt.Sprintf("kitdtest%d", os.Getpid())
	t.Setenv("TMPDIR", "/tmp")
	t.Setenv("XDG_RUNTIME_DIR", "")

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(socketPath)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(socketPath)) })
	assert.Equal(filepath.Join(filepath.Dir(socketPath), "daemon.sock"), socketPath)

	info, err := os.Stat(filepath.Dir(socketPath))
	require.NoError(err)
	assert.True(info.IsDir())
	if runtime.GOOS != "windows" {
		assert.Zero(info.Mode().Perm() & 0o077)
	}
}

func TestDefaultSocketPathRejectsPathLikeServiceNames(t *testing.T) {
	cases := map[string]string{
		"current-directory": ".",
		"parent-directory":  "..",
		"slash":             "bad/service",
		"backslash":         `bad\service`,
		"colon":             "bad:service",
		"space":             "bad service",
		"null":              string([]byte{'b', 0, 'd'}),
	}
	for name, service := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, daemon.DefaultSocketPath(service))
		})
	}
}
