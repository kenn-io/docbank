package embedlocal

import (
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestEndpointOllamaFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		backend string
	}{
		{"latest", 200, "ollama"},
		{"status", 503, "local"},
		{"absent model", 404, "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !assert.Equal(t, http.MethodPost, r.Method) || !assert.Equal(t, "/api/show", r.URL.Path) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request struct {
					Model string `json:"model"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) || !assert.Equal(t, "nomic-embed-text", request.Model) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer ollama.Close()
			dir := t.TempDir()
			backends := map[string]string{"local": localServer(t, dir, Version).URL + "/v1", "ollama": ollama.URL + "/v1"}
			got, err := endpoint(t.Context(), Options{DataDir: dir}, ollama.URL, 2*time.Minute)
			require.NoError(t, err)
			assert.Equal(t, backends[tc.backend], got)
		})
	}
}

func TestEndpointNotFound(t *testing.T) {
	ollama := httptest.NewServer(http.NotFoundHandler())
	defer ollama.Close()
	dir := t.TempDir()
	_, err := endpoint(t.Context(), Options{DataDir: dir}, ollama.URL, 2*time.Minute)
	require.ErrorIs(t, err, ErrNotFound)
	localServer(t, dir, "2")
	_, err = endpoint(t.Context(), Options{DataDir: dir}, ollama.URL, 2*time.Minute)
	require.ErrorIs(t, err, ErrNotFound)
}

func localServer(t *testing.T, dir, version string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{Service: Service, Version: version}))
	t.Cleanup(server.Close)
	ep, err := daemon.ParseEndpoint(server.URL, daemon.ParseEndpointOptions{TCPPolicy: daemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (daemon.RuntimeStore{Dir: dir, Prefix: Service}).Write(daemon.NewRuntimeRecord(Service, version, ep))
	require.NoError(t, err)
	return server
}

func TestEndpointEarlyExit(t *testing.T) {
	ollama := httptest.NewServer(http.NotFoundHandler())
	defer ollama.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "exit"), nil, 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Cleanup(func() { waitForChildren(t, dir) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = endpoint(ctx, Options{DataDir: dir, Executable: executable}, ollama.URL, 0)
	require.ErrorContains(t, err, "ONNX Runtime library missing")
	require.ErrorContains(t, err, filepath.Join(dir, "kennembed.log"))
	assert.NotErrorIs(t, err, context.DeadlineExceeded, "child exit should end startup before the 5s context deadline")
}

func TestEndpointStartLockTimeoutExcludesStaleLog(t *testing.T) {
	ollama := httptest.NewServer(http.NotFoundHandler())
	defer ollama.Close()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kennembed.log")
	require.NoError(t, os.WriteFile(logPath, []byte("OLD FAILURE FROM LAST WEEK\n"), 0o600))
	store := daemon.RuntimeStore{Dir: dir, Prefix: Service}
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(t, err)
	require.True(t, acquired)
	defer release()
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond) //nolint:kennlint // deadlinetest: the held start lock makes the caller deadline the expected result
	defer cancel()
	_, err = endpoint(ctx, Options{DataDir: dir, Executable: executable}, ollama.URL, 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, logPath)
	assert.NotContains(t, err.Error(), "OLD FAILURE")
}

func TestEndpointJoinStartup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"reuse", http.StatusOK}, {"failed", http.StatusServiceUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			opts, ollama, entered, ready := startupFixture(t, tc.status)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			first := make(chan error, 1)
			go func() { _, err := endpoint(ctx, opts, ollama, 0); first <- err }()
			var pid int
			select {
			case pid = <-entered:
			case err := <-first:
				require.FailNow(t, "initial caller returned before child registration", "%v", err)
			case <-ctx.Done():
				require.FailNow(t, "child did not register before the 5s context deadline", "%v", ctx.Err())
			}
			cancel()
			firstErr := <-first
			require.ErrorIs(t, firstErr, context.Canceled)
			joining, stopJoining := context.WithTimeout(t.Context(), 5*time.Second)
			defer stopJoining()
			done := make(chan endpointResult, 1)
			go func() {
				url, err := endpoint(joining, opts, ollama, 0)
				done <- endpointResult{url, err}
			}()
			store := daemon.RuntimeStore{Dir: opts.DataDir, Prefix: Service}
			waitForStartup(t, store)
			assert.Never(t, func() bool {
				launches, err := filepath.Glob(filepath.Join(opts.DataDir, "launch.*"))
				return err != nil || len(launches) != 1
			}, time.Second, 10*time.Millisecond, "loading must not launch another child")
			ready()
			result := <-done
			require.NoError(t, joining.Err(), "shared startup must finish before the 5s context deadline")
			if tc.status == http.StatusServiceUnavailable {
				require.ErrorContains(t, result.err, "cache model.onnx: download: HTTP 503")
			} else {
				require.NoError(t, result.err)
				records, err := store.List()
				require.NoError(t, err)
				require.Len(t, records, 1)
				assert.Equal(t, pid, records[0].PID)
				assert.Equal(t, records[0].Endpoint().BaseURL()+"/v1", result.url)
			}
		})
	}
}

type endpointResult struct {
	url string
	err error
}

func startupFixture(t *testing.T, status int) (Options, string, <-chan int, func()) {
	t.Helper()
	ollama := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ollama.Close)
	dir := t.TempDir()
	entered, release := make(chan int, 1), make(chan struct{})
	ready := sync.OnceFunc(func() { close(release) })
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
		select {
		case entered <- pid:
		default:
		}
		<-release
		w.WriteHeader(status)
	}))
	t.Cleanup(gate.Close)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte(gate.URL), 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Cleanup(func() { ready(); waitForChildren(t, dir) })
	return Options{DataDir: dir, Executable: executable}, ollama.URL, entered, ready
}

func waitForChildren(t *testing.T, dir string) {
	t.Helper()
	launches, err := filepath.Glob(filepath.Join(dir, "launch.*"))
	require.NoError(t, err)
	for _, launch := range launches {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(launch), "launch."))
		require.NoError(t, err)
		var address string
		require.Eventually(t, func() bool {
			if !daemon.ProcessAlive(pid) {
				return true
			}
			records, err := (daemon.RuntimeStore{Dir: dir, Prefix: Service}).List()
			if err != nil {
				return false
			}
			for _, record := range records {
				if record.PID == pid {
					address = record.Endpoint().BaseURL()
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond)
		if address != "" {
			req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodPost, address+"/stop", nil)
			require.NoError(t, err)
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			require.NoError(t, err)
			resp.Body.Close()
		}
		require.Eventually(t, func() bool { return !daemon.ProcessAlive(pid) }, 5*time.Second, 10*time.Millisecond)
	}
}

func waitForStartup(t *testing.T, store daemon.RuntimeStore) {
	t.Helper()
	require.Eventually(t, func() bool {
		unlock, acquired, err := store.TryAcquireStartLock(t.Context())
		if acquired {
			unlock()
		}
		return err == nil && !acquired
	}, 5*time.Second, time.Millisecond)
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-data-dir" {
		os.Exit(runServer(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runServer(args []string) int {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	dir := flags.String("data-dir", "", "")
	listen := flags.String("listen", "", "")
	if flags.Parse(args) != nil || !filepath.IsAbs(*dir) || *listen != "127.0.0.1:0" {
		return 2
	}
	if err := os.WriteFile(filepath.Join(*dir, fmt.Sprintf("launch.%d", os.Getpid())), nil, 0o600); err != nil {
		return 3
	}
	store := daemon.RuntimeStore{Dir: *dir, Prefix: Service}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := store.AcquireOwnerLock(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 11
	}
	defer release()
	if _, err := os.Stat(filepath.Join(*dir, "exit")); err == nil {
		fmt.Fprintln(os.Stderr, "ONNX Runtime library missing")
		return 12
	}
	if gate, err := os.ReadFile(filepath.Join(*dir, "gate")); err == nil {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("%s?pid=%d", gate, os.Getpid()), nil)
		client := &http.Client{Timeout: 3 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			return 8
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintln(os.Stderr, "cache model.onnx: download: HTTP 503")
			return 8
		}
	}
	listener, err := new(net.ListenConfig).Listen(context.Background(), "tcp", *listen)
	if err != nil {
		return 5
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: Service, Version: Version}))
	stop := make(chan struct{})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush()
		close(stop)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	_, err = (daemon.RuntimeStore{Dir: *dir, Prefix: Service}).Write(daemon.NewRuntimeRecord(Service, Version, daemon.Endpoint{Network: daemon.NetworkTCP, Address: listener.Addr().String()}))
	if err != nil {
		return 6
	}
	<-stop
	return 0
}
