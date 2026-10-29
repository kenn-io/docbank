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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	docmcp "go.kenn.io/docbank/internal/mcp"
	"go.kenn.io/kit/safefileio"
)

func daemonMCPEnvironment(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_API_PORT", "0")
	t.Setenv("DOCBANK_BIND_ADDR", "127.0.0.1")
	for _, name := range []string{"DOCBANK_API_KEY", "DOCBANK_API_KEY_FILE", "DOCBANK_ALLOWED_HOSTS",
		"DOCBANK_MCP_HTTP_ENABLED", "DOCBANK_MCP_HTTP_PORT", "DOCBANK_MCP_HTTP_TOKEN", "DOCBANK_MCP_HTTP_TOKEN_FILE",
		"DOCBANK_MCP_HTTP_ALLOWED_HOSTS"} {
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

func postMCPVaultInfo(t *testing.T, url, host, bearer string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":%q,"io.modelcontextprotocol/clientCapabilities":{}},"name":"get_vault_info","arguments":{}}}`, docmcp.ProtocolVersion)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", docmcp.ProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "get_vault_info")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(data)
}

func TestDaemonMCPHTTPReadsSameVaultAndShutsDown(t *testing.T) {
	for _, test := range []struct {
		name     string
		separate bool
	}{{name: "shared API port"}, {name: "separate port", separate: true}} {
		t.Run(test.name, func(t *testing.T) {
			root := daemonMCPEnvironment(t)
			separate := ""
			if test.separate {
				separate = unusedMCPAddress(t)
				_, port, err := net.SplitHostPort(separate)
				require.NoError(t, err)
				t.Setenv("DOCBANK_MCP_HTTP_PORT", port)
			}
			// Hosting is on by default for a loopback API once the token resolves.
			t.Setenv("DOCBANK_MCP_HTTP_TOKEN", "synthetic-mcp-bearer")
			t.Setenv("DOCBANK_MCP_HTTP_ALLOWED_HOSTS", "docbank:7341")
			if test.separate {
				t.Setenv("DOCBANK_MCP_HTTP_ENABLED", "true")
			}
			stop := startServe(t)
			record := waitForDaemon(t, root)
			connection := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
			info, err := connection.API().VaultInfo(t.Context())
			require.NoError(t, err)
			mcpAddress := record.Address
			if test.separate {
				mcpAddress = separate
			}
			for _, call := range []struct {
				name, bearer, host string
				status             int
			}{
				{"valid", "synthetic-mcp-bearer", "docbank:7341", http.StatusOK},
				{"API key", record.Metadata["api_key"], "docbank:7341", http.StatusUnauthorized},
				{"unlisted Host", "synthetic-mcp-bearer", "unlisted.example", http.StatusForbidden},
			} {
				status, data := postMCPVaultInfo(t, "http://"+mcpAddress+"/mcp", call.host, call.bearer)
				require.Equal(t, call.status, status, "%s: %s", call.name, data)
				if call.status == http.StatusOK {
					var wire struct {
						Result struct {
							IsError bool `json:"isError"`
							Content struct {
								VaultID string `json:"vault_id"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					require.NoError(t, json.Unmarshal([]byte(data), &wire))
					assert.False(t, wire.Result.IsError, data)
					assert.Equal(t, info.VaultID, wire.Result.Content.VaultID, data)
				}
			}
			// The MCP bearer never authorizes the API beside it.
			_, err = daemonconn.New("http://"+record.Address, "synthetic-mcp-bearer").API().VaultInfo(t.Context())
			require.Error(t, err)
			stop()
			addresses := []string{record.Address}
			if test.separate {
				addresses = append(addresses, separate)
			}
			assertDaemonMCPCleanedUp(t, root, addresses...)
		})
	}
}

func TestDaemonMCPHTTPStartupFailuresReleaseOwnership(t *testing.T) {
	for _, test := range []struct{ name, config, token, port, message string }{
		{name: "invalid port", port: "70000", message: "DOCBANK_MCP_HTTP_PORT"},
		{name: "unknown config", config: "[mcp.http]\nbogus=true\n", message: "unknown key"},
		{name: "missing binding", message: "credential_binding is required"},
		{name: "equal API key", token: "synthetic-api-key", message: "must differ"},
		{name: "occupied port", token: "synthetic-mcp-bearer", message: "listen for MCP HTTP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := daemonMCPEnvironment(t)
			t.Setenv("DOCBANK_API_KEY", "synthetic-api-key")
			t.Setenv("DOCBANK_MCP_HTTP_ENABLED", "true")
			if test.name == "occupied port" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				_, test.port, err = net.SplitHostPort(listener.Addr().String())
				require.NoError(t, err)
			}
			t.Setenv("DOCBANK_MCP_HTTP_PORT", test.port)
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

func TestDaemonMCPHTTPNotHostedWithoutUsableCredentialOrWhenDisabled(t *testing.T) {
	for _, test := range []struct{ name, enabled, tokenFile, token string }{
		{name: "default with unreadable credential", tokenFile: "missing"},
		{name: "explicitly disabled", enabled: "false", token: "synthetic-mcp-bearer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := daemonMCPEnvironment(t)
			t.Setenv("DOCBANK_MCP_HTTP_ENABLED", test.enabled)
			t.Setenv("DOCBANK_MCP_HTTP_TOKEN", test.token)
			if test.tokenFile != "" {
				t.Setenv("DOCBANK_MCP_HTTP_TOKEN_FILE", filepath.Join(root, test.tokenFile))
			}
			stop := startServe(t)
			record := waitForDaemon(t, root)
			status, data := postMCPVaultInfo(t, "http://"+record.Address+"/mcp", record.Address, "synthetic-mcp-bearer")
			assert.Equal(t, http.StatusUnauthorized, status, data)
			stop()
			assertDaemonMCPCleanedUp(t, root, record.Address)
		})
	}
}

func TestDaemonMCPHTTPUsesEnvironmentOverriddenPort(t *testing.T) {
	root := daemonMCPEnvironment(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[mcp.http]\nport = 70000\n"), 0o600))
	t.Setenv("DOCBANK_MCP_HTTP_PORT", "0")
	t.Setenv("DOCBANK_MCP_HTTP_ENABLED", "true")
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN", "synthetic-mcp-bearer")
	stop := startServe(t)
	record := waitForDaemon(t, root)
	status, data := postMCPVaultInfo(t, "http://"+record.Address+"/mcp", record.Address, "synthetic-mcp-bearer")
	assert.Equal(t, http.StatusOK, status, data)
	stop()
	assertDaemonMCPCleanedUp(t, root, record.Address)
}

func TestDaemonMCPHTTPDisablesBackgroundIdleTimeout(t *testing.T) {
	root := daemonMCPEnvironment(t)
	t.Setenv(daemonconn.EnvBackgroundDaemon, "1")
	t.Setenv("DOCBANK_MCP_HTTP_TOKEN", "synthetic-mcp-bearer")
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[server]\nidle_timeout = \"30ms\"\n"), 0o600))
	stop := startServe(t)
	record := waitForDaemon(t, root)
	time.Sleep(250 * time.Millisecond) //nolint:kennlint // sleeptest: the daemon's idle ticker runs on the wall clock behind a real loopback listener, so synctest can't host it
	_, _, found, err := daemonconn.Find(t.Context(), root)
	require.NoError(t, err)
	assert.True(t, found, "hosted MCP must keep a background daemon alive")
	stop()
	assertDaemonMCPCleanedUp(t, root, record.Address)
}

func TestDaemonMCPHTTPDrainKeepsAPIAvailable(t *testing.T) {
	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	apiHTTP := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("synthetic-vault")) })}
	mcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started, release := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseRead)
	mcpHTTP := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		request, requestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+apiListener.Addr().String(), nil)
		if requestErr != nil {
			http.Error(w, "request failed", http.StatusInternalServerError)
			return
		}
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			http.Error(w, "API unavailable", http.StatusInternalServerError)
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(w, response.Body)
	})}
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- serveDaemonHTTP(t.Context(), stop, []servingHTTP{
			{name: "API", server: apiHTTP, listener: apiListener},
			{name: "MCP HTTP", server: mcpHTTP, listener: mcpListener, drainedFirst: true},
		}, drainHTTPServer(mcpHTTP), func() {}, func(context.Context) error { return nil })
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

func TestDaemonMCPHTTPSharedDrainKeepsAPIAvailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	base := "http://" + listener.Addr().String()
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("synthetic-vault")) })
	started, release := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseRead)
	var once sync.Once
	gate := docmcp.NewRequestGate()
	mcp := gate.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture := false
		once.Do(func() { fixture = true; close(started) })
		if !fixture {
			// A probe admitted before the drain started returns at once.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		<-release
		request, requestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/api", nil)
		if requestErr != nil {
			http.Error(w, "request failed", http.StatusInternalServerError)
			return
		}
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			http.Error(w, "API unavailable", http.StatusInternalServerError)
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(w, response.Body)
	}))
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: withMCPRoute(api, mcp)}
	t.Cleanup(func() { _ = server.Close() })
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- serveDaemonHTTP(t.Context(), stop, []servingHTTP{{name: "API", server: server, listener: listener}},
			gate.Shutdown, func() {}, func(context.Context) error { return nil })
	}()
	result := make(chan string, 1)
	go func() {
		response, err := http.Post(base+"/mcp", "application/json", strings.NewReader("{}"))
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
	// Once the drain starts, new MCP requests are refused on the shared port.
	probe := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		response, err := probe.Post(base+"/mcp", "application/json", strings.NewReader("{}"))
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusServiceUnavailable
	}, time.Second*5, time.Millisecond*10)
	releaseRead()
	assert.Equal(t, "synthetic-vault", <-result)
	require.NoError(t, <-done)
}

func TestDaemonMCPHTTPSharedDrainIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started, canceled := make(chan struct{}), make(chan struct{})
	gate := docmcp.NewRequestGate()
	mcp := gate.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: withMCPRoute(http.NotFoundHandler(), mcp)}
	t.Cleanup(func() { _ = server.Close() })
	stop, done := make(chan struct{}), make(chan error, 1)
	var apiShutdownErr error
	go func() {
		done <- serveDaemonHTTP(t.Context(), stop, []servingHTTP{{name: "API", server: server, listener: listener}},
			gate.Shutdown, func() {}, func(ctx context.Context) error {
				// The gate must cancel the stalled read before the API deadline.
				select {
				case <-canceled:
				case <-ctx.Done():
				}
				apiShutdownErr = ctx.Err()
				return nil
			})
	}()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Post("http://"+listener.Addr().String()+"/mcp", "application/json", strings.NewReader("{}"))
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	<-started
	close(stop)
	err = <-done
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "draining daemon MCP HTTP requests")
	require.NoError(t, apiShutdownErr)
	<-requestDone
}

func TestDaemonMCPHTTPDrainIsBoundedAndClosesActiveRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started, canceled, jobsStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	jobsStoppedBeforeCancel := false
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		select {
		case <-jobsStopped:
			jobsStoppedBeforeCancel = true
		default:
		}
		close(canceled)
	})}
	t.Cleanup(func() { _ = server.Close() })
	stop, done := make(chan struct{}), make(chan error, 1)
	var apiShutdownErr error
	go func() {
		done <- serveDaemonHTTP(t.Context(), stop,
			[]servingHTTP{{name: "MCP HTTP", server: server, listener: listener, drainedFirst: true}},
			drainHTTPServer(server), func() { close(jobsStopped) },
			func(ctx context.Context) error { apiShutdownErr = ctx.Err(); return nil })
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
	// A stalled MCP read must leave the API its own share of the deadline.
	require.NoError(t, apiShutdownErr)
	<-canceled
	assert.True(t, jobsStoppedBeforeCancel, "jobs stop before the MCP drain")
	<-requestDone
}

func TestDaemonMCPHTTPServeFailurePropagatesAndDrainsAPI(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = apiListener.Close() })
	mcpHTTP, apiHTTP := &http.Server{ReadHeaderTimeout: 10 * time.Second}, &http.Server{ReadHeaderTimeout: 10 * time.Second}
	err = serveDaemonHTTP(t.Context(), nil, []servingHTTP{
		{name: "API", server: apiHTTP, listener: apiListener},
		{name: "MCP HTTP", server: mcpHTTP, listener: listener, drainedFirst: true},
	}, drainHTTPServer(mcpHTTP), func() {}, func(context.Context) error { return nil })
	require.ErrorIs(t, err, net.ErrClosed)
	require.ErrorContains(t, err, "daemon MCP HTTP server")
	// Shutdown can precede the API goroutine's entry into Serve.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		rebound, bindErr := net.Listen("tcp", apiListener.Addr().String())
		if assert.NoError(c, bindErr) {
			assert.NoError(c, rebound.Close())
		}
	}, daemonShutdownTimeout, 10*time.Millisecond)
}

func TestDaemonMCPHTTPTOMLPortSelectsMountedBinding(t *testing.T) {
	root := daemonMCPEnvironment(t)
	address := unusedMCPAddress(t)
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	secret := filepath.Join(root, "mcp-bearer")
	file, err := safefileio.CreatePrivateFile(secret)
	require.NoError(t, err)
	_, err = file.WriteString("synthetic-mounted-bearer\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	t.Setenv("SYNTHETIC_MCP_BEARER_FILE", secret)
	t.Setenv("SYNTHETIC_MCP_BEARER", "")
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`[mcp.http]
enabled = true
port = `+port+`
credential_binding = "credential:inbound"
[credential_bindings.inbound]
environment_variable = "SYNTHETIC_MCP_BEARER"
`), 0o600))
	stop := startServe(t)
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
