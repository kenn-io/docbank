package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	docmcp "go.kenn.io/docbank/internal/mcp"
)

func daemonMCPEnvironment(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_API_PORT", "0")
	t.Setenv("DOCBANK_BIND_ADDR", "127.0.0.1")
	for _, name := range []string{"DOCBANK_API_KEY", "DOCBANK_API_KEY_FILE", "DOCBANK_ALLOWED_HOSTS",
		"DOCBANK_MCP_HTTP_LISTEN", "DOCBANK_MCP_HTTP_TOKEN", "DOCBANK_MCP_HTTP_TOKEN_FILE", "DOCBANK_MCP_HTTP_ALLOWED_HOSTS"} {
		t.Setenv(name, "")
	}
	return root
}

func unusedMCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func assertDaemonMCPCleanedUp(t *testing.T, root string, addresses ...string) {
	t.Helper()
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err, "released listener %s", address)
		require.NoError(t, listener.Close())
	}
	records, err := daemonconn.RuntimeStore(root).List()
	require.NoError(t, err)
	assert.Empty(t, records)
	layout := home.Layout{Root: root}
	heldRoot, lock, err := layout.OpenAndLockExclusive()
	require.NoError(t, err, "vault ownership must be released")
	require.NoError(t, lock.Release())
	require.NoError(t, heldRoot.Close())
}

func TestDaemonMCPHTTPReadsSameVaultAndShutsDown(t *testing.T) {
	root := daemonMCPEnvironment(t)
	address := unusedMCPAddress(t)
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	t.Setenv("DOCBANK_MCP_HTTP_LISTEN", "0.0.0.0:"+port)
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN", "synthetic-mcp-bearer")
	t.Setenv("DOCBANK_MCP_HTTP_ALLOWED_HOSTS", "docbank:7341")
	stop := startServe(t)
	record := waitForDaemon(t, root)
	connection := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	info, err := connection.API().VaultInfo(t.Context())
	require.NoError(t, err)
	for _, test := range []struct {
		name, bearer, host string
		status             int
	}{
		{"valid", "synthetic-mcp-bearer", "docbank:7341", 200},
		{"API key", record.Metadata["api_key"], "docbank:7341", 401},
		{"unlisted Host", "synthetic-mcp-bearer", "unlisted.example", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":%q,"io.modelcontextprotocol/clientCapabilities":{}},"name":"get_vault_info","arguments":{}}}`, docmcp.ProtocolVersion)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/mcp", strings.NewReader(body))
			require.NoError(t, err)
			request.Host = test.host
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("Mcp-Protocol-Version", docmcp.ProtocolVersion)
			request.Header.Set("Mcp-Method", "tools/call")
			request.Header.Set("Mcp-Name", "get_vault_info")
			request.Header.Set("Authorization", "Bearer "+test.bearer)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, test.status, response.StatusCode, string(data))
			if test.status == http.StatusOK {
				var wire struct {
					Result struct {
						IsError bool `json:"isError"`
						Content struct {
							VaultID string `json:"vault_id"`
						} `json:"structuredContent"`
					} `json:"result"`
				}
				require.NoError(t, json.Unmarshal(data, &wire))
				assert.False(t, wire.Result.IsError, string(data))
				assert.Equal(t, info.VaultID, wire.Result.Content.VaultID, string(data))
			}
		})
	}
	stop()
	assertDaemonMCPCleanedUp(t, root, record.Address, address)
}

func TestDaemonMCPHTTPStartupFailuresReleaseOwnership(t *testing.T) {
	for _, test := range []struct{ name, config, token, listen, message string }{
		{name: "invalid listen", listen: "localhost:7341", message: "explicit IP"},
		{name: "unknown config", config: "[mcp.http]\nbogus=true\n", message: "unknown key"},
		{name: "missing binding", message: "credential_binding is required"},
		{name: "equal API key", token: "synthetic-api-key", message: "must differ"},
		{name: "occupied port", token: "synthetic-mcp-bearer", message: "listen for MCP HTTP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := daemonMCPEnvironment(t)
			t.Setenv("DOCBANK_API_KEY", "synthetic-api-key")
			listen := test.listen
			if listen == "" {
				listen = "127.0.0.1:0"
			}
			if test.name == "occupied port" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				listen = listener.Addr().String()
			}
			t.Setenv("DOCBANK_MCP_HTTP_LISTEN", listen)
			t.Setenv("DOCBANK_MCP_HTTP_TOKEN", test.token)
			if test.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(test.config), 0o600))
			}
			err := runServe(t.Context())
			require.ErrorContains(t, err, test.message)
			assert.NotContains(t, err.Error(), "synthetic-api-key")
			assert.NotContains(t, err.Error(), "synthetic-mcp-bearer")
			assertDaemonMCPCleanedUp(t, root)
		})
	}
}

func TestDaemonMCPHTTPListenFlagOverridesEnvironmentAndTOML(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			root := daemonMCPEnvironment(t)
			require.NotNil(t, daemonRunCmd.Flags().Lookup("mcp-http-listen"))
			if explicit {
				require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[mcp.http]\nlisten='127.0.0.1:7341'\n"), 0o600))
				t.Setenv("DOCBANK_MCP_HTTP_LISTEN", "invalid-listen")
			}
			// With no listener, the selected but missing secret stays unread.
			t.Setenv("DOCBANK_MCP_HTTP_TOKEN_FILE", filepath.Join(root, "missing"))
			stop := startServeWith(t, func(ctx context.Context) error {
				cmd := &cobra.Command{}
				cmd.SetContext(ctx)
				cmd.Flags().String("mcp-http-listen", "", "")
				if explicit {
					if err := cmd.Flags().Parse([]string{"--mcp-http-listen="}); err != nil {
						return err
					}
				}
				return daemonRunCmd.RunE(cmd, nil)
			})
			record := waitForDaemon(t, root)
			stop()
			assertDaemonMCPCleanedUp(t, root, record.Address)
		})
	}
}

func TestDaemonMCPHTTPDrainKeepsAPIAvailable(t *testing.T) {
	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	apiHTTP := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("synthetic-vault")) })}
	mcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started, release := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseRead)
	mcpHTTP := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		request, requestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+apiListener.Addr().String(), nil)
		if requestErr != nil {
			http.Error(w, "request failed", 500)
			return
		}
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			http.Error(w, "API unavailable", 500)
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(w, response.Body)
	})}
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- serveDaemonHTTP(t.Context(), stop, []servingHTTP{{"API", apiHTTP, apiListener}, {"MCP HTTP", mcpHTTP, mcpListener}}, mcpHTTP,
			func(context.Context) error { return nil })
	}()
	t.Cleanup(func() { _ = mcpHTTP.Close(); _ = apiHTTP.Close() })
	result := make(chan string, 1)
	go func() {
		response, err := http.Get("http://" + mcpListener.Addr().String())
		if err != nil {
			result <- err.Error()
			return
		}
		defer func() { _ = response.Body.Close() }()
		data, _ := io.ReadAll(response.Body)
		result <- string(data)
	}()
	<-started
	close(stop)
	// Once Shutdown closes the MCP listener, the pending read can use the API.
	require.Eventually(t, func() bool {
		connection, err := net.DialTimeout("tcp", mcpListener.Addr().String(), time.Second)
		if err != nil {
			return true
		}
		_ = connection.Close()
		return false
	}, time.Second*5, time.Millisecond*10)
	releaseRead()
	assert.Equal(t, "synthetic-vault", <-result)
	require.NoError(t, <-done)
}

func TestDaemonMCPHTTPDrainIsBoundedAndClosesActiveRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started, canceled := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	})}
	t.Cleanup(func() { _ = server.Close() })
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- serveDaemonHTTP(t.Context(), stop, []servingHTTP{{"MCP HTTP", server, listener}}, server,
			func(ctx context.Context) error { return ctx.Err() })
	}()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	<-started
	close(stop)
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	<-canceled
	<-requestDone
}

func TestDaemonMCPHTTPServeFailurePropagatesAndDrainsAPI(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mcpHTTP, apiHTTP := &http.Server{}, &http.Server{}
	err = serveDaemonHTTP(t.Context(), nil,
		[]servingHTTP{{"API", apiHTTP, apiListener}, {"MCP HTTP", mcpHTTP, listener}}, mcpHTTP,
		func(context.Context) error { return nil })
	require.ErrorIs(t, err, net.ErrClosed)
	require.ErrorContains(t, err, "daemon MCP HTTP server")
	rebound, err := net.Listen("tcp", apiListener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}

func TestDaemonMCPHTTPListenFlagSelectsMountedBinding(t *testing.T) {
	root := daemonMCPEnvironment(t)
	address := unusedMCPAddress(t)
	secret := filepath.Join(root, "mcp-bearer")
	require.NoError(t, os.WriteFile(secret, []byte("synthetic-mounted-bearer\n"), 0o600))
	t.Setenv("SYNTHETIC_MCP_BEARER_FILE", secret)
	t.Setenv("SYNTHETIC_MCP_BEARER", "")
	t.Setenv("DOCBANK_MCP_HTTP_LISTEN", "invalid-env-listen")
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[mcp.http]
listen = "invalid-toml-listen"
credential_binding = "credential:inbound"
[credential_bindings.inbound]
environment_variable = "SYNTHETIC_MCP_BEARER"
`), 0o600))
	stop := startServeWith(t, func(ctx context.Context) error {
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.Flags().String("mcp-http-listen", "", "")
		if err := cmd.Flags().Parse([]string{"--mcp-http-listen=" + address}); err != nil {
			return err
		}
		return daemonRunCmd.RunE(cmd, nil)
	})
	record := waitForDaemon(t, root)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/mcp", strings.NewReader("{}"))
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	stop()
	assertDaemonMCPCleanedUp(t, root, record.Address, address)
}
