package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMCPOriginalDownloadAmbiguousTickets(t *testing.T) {
	var acquisitions atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		return nil, errDaemonUnavailable
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-bearer"})
	require.NoError(t, err)
	for _, query := range []string{"", "ticket=" + strings.Repeat("A", 43) + "&ticket=" + strings.Repeat("A", 43)} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/mcp/original?"+query, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		require.Contains(t, w.Body.String(), `"code":"download_not_found"`)
	}
	require.Zero(t, acquisitions.Load())
}

func TestMCPOriginalDownloadAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var acquisitions atomic.Int32
		lease := newDaemonLeaseWith(func(ctx context.Context) (*daemonconn.Connection, error) {
			acquisitions.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}, func(*daemonconn.Connection) error { return nil })
		server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
		handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-bearer", limits: httpLimits{MaxConcurrentRequests: 1}})
		require.NoError(t, err)
		start := func(ctx context.Context) <-chan int {
			done := make(chan int, 1)
			go func() {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/mcp/original?ticket=junk", nil))
				done <- w.Code
			}()
			return done
		}
		ctx, cancel := context.WithCancel(t.Context())
		first := start(ctx)
		synctest.Wait()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/mcp/original?ticket=junk", nil))
		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.EqualValues(t, 1, acquisitions.Load())
		cancel()
		require.Equal(t, http.StatusBadGateway, <-first)
		second := start(t.Context())
		synctest.Wait()
		require.EqualValues(t, 2, acquisitions.Load())
		time.Sleep(daemonAcquisitionTimeout)
		require.Equal(t, http.StatusBadGateway, <-second)
		third := start(t.Context())
		synctest.Wait()
		require.EqualValues(t, 3, acquisitions.Load())
		time.Sleep(daemonAcquisitionTimeout)
		require.Equal(t, http.StatusBadGateway, <-third)
	})
}

func TestMCPOriginalDownloadStalledConsumerReleasesSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "original bytes")
		}))
		previous := http.DefaultTransport
		http.DefaultTransport = upstream.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = previous })
		server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, upstream.URL))
		handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-bearer", limits: httpLimits{MaxConcurrentRequests: 1}})
		require.NoError(t, err)
		writer := newDeadlineResponseWriter(true)
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(writer, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/mcp/original?ticket="+strings.Repeat("A", 43), nil))
			close(done)
		}()
		synctest.Wait()
		time.Sleep(originalDownloadIdleTimeout)
		synctest.Wait()
		select {
		case <-done:
		default:
			close(writer.writeExpired)
			<-done
			t.Fatal("stalled consumer retained the download slot")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1/mcp/original?ticket="+strings.Repeat("A", 43), nil))
		require.Equal(t, http.StatusOK, recorder.Code)
	})
}

type delayedOriginalReader struct {
	ctx   context.Context
	delay time.Duration
}

func (r *delayedOriginalReader) Read(p []byte) (int, error) {
	select {
	case <-time.After(r.delay):
		p[0] = 'x'
		return 1, nil
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

func (*delayedOriginalReader) Close() error { return nil }

func TestMCPOriginalDownloadSlowAndStalledReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		reader := &delayedOriginalReader{ctx: ctx, delay: 3 * time.Minute}
		timer := time.AfterFunc(originalDownloadIdleTimeout, cancel)
		timer.Stop()
		body := &originalDownloadBody{ReadCloser: reader, cancel: cancel, closeConnection: func() {}, timer: timer}
		defer func() { require.NoError(t, body.Close()) }()
		var buf [1]byte
		n, err := body.Read(buf[:])
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Equal(t, byte('x'), buf[0])
		reader.delay = 6 * time.Minute
		_, err = body.Read(buf[:])
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestMCPOriginalDownloadAcquiresSeparateConnections(t *testing.T) {
	f := newNativeExportFixture(t)
	selected := f.addFile(t, "synthetic.bin", strings.Repeat("synthetic-media", 1<<18))
	var acquisitions atomic.Int32
	connection := f.provenConnection(t)
	lease := newDaemonLeaseWith(func(ctx context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		fresh, running, err := daemonconn.Running(ctx)
		require.NoError(t, err)
		require.True(t, running)
		t.Cleanup(func() { require.NoError(t, fresh.Close()) })
		return fresh, err
	}, func(c *daemonconn.Connection) error { return c.Close() })
	lease.daemonconn = connection
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
		"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size}
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-review-bearer"})
	require.NoError(t, err)
	addresses := make([]string, 2)
	for i := range addresses {
		result := exportCall(t, server, "get_original_download_url", args)
		// Stdio capability and HTTP relay share the same daemon-issued ticket.
		direct := conflictField[string](t, result, "url")
		_, query, ok := strings.Cut(direct, "?")
		require.True(t, ok)
		addresses[i] = "http://127.0.0.1/mcp/original?" + query
	}
	for _, address := range addresses {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, address, nil))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Len(t, response.Body.Bytes(), int(selected.Size))
	}
	require.EqualValues(t, 2, acquisitions.Load())
}

func TestMCPOriginalDownloadURL(t *testing.T) {
	f := newNativeExportFixture(t)
	content := "synthetic-original\x00"
	selected := f.addFile(t, "original.bin", content)
	args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
		"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size}
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	// Stdio returns a directly downloadable daemon capability without a web app.
	result := exportCall(t, server, "get_original_download_url", args)
	address := conflictField[string](t, result, "url")
	require.True(t, strings.HasPrefix(address, f.address+"/api/daemon/web-download/file?ticket="))

	for _, mount := range []string{"standalone", "dedicated-port"} {
		t.Run(mount, func(t *testing.T) {
			transportServer := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{},
				newDaemonLeaseWith(f.lease.ensure, f.lease.close))
			options := HTTPOptions{BearerToken: "synthetic-mcp-bearer", AllowedHosts: []string{"docbank:7341"}}
			var handler http.Handler
			var err error
			switch mount {
			case "standalone":
				handler, err = transportServer.HTTPTransportHandler(options)
			case "dedicated-port":
				httpServer, listener, listenErr := ListenHTTP(t.Context(), transportServer, "127.0.0.1:0", options)
				require.NoError(t, listenErr)
				done := make(chan error, 1)
				go func() { done <- httpServer.Serve(listener) }()
				t.Cleanup(func() { require.NoError(t, httpServer.Close()); require.ErrorIs(t, <-done, http.ErrServerClosed) })
				target, parseErr := url.Parse("http://" + listener.Addr().String())
				require.NoError(t, parseErr)
				handler = httputil.NewSingleHostReverseProxy(target)
			}
			require.NoError(t, err)
			call := func() string {
				raw := requestFor("tools/call", map[string]any{"name": "get_original_download_url", "arguments": args})
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://docbank:7341/mcp", bytes.NewReader(raw))
				req.Header = protocolHeaders("tools/call", "get_original_download_url")
				req.Header.Set("Authorization", "Bearer synthetic-mcp-bearer")
				req.Header.Set("X-Forwarded-Host", "foreign.test")
				req.Header.Set("X-Forwarded-Proto", "https")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				out := objectField(t, decodeResult(t, recorder.Body.Bytes()), "structuredContent")
				address := conflictField[string](t, out, "url")
				require.Equal(t, strings.TrimPrefix(address, "http://docbank:7341"), out["path"])
				endpoint, err := url.Parse("https://docbank:7341/mcp")
				require.NoError(t, err)
				path, err := url.Parse(conflictField[string](t, out, "path"))
				require.NoError(t, err)
				require.Equal(t, strings.Replace(address, "http:", "https:", 1), endpoint.ResolveReference(path).String())
				parsed, err := url.Parse(address)
				require.NoError(t, err)
				require.Equal(t, "docbank:7341", parsed.Host)
				require.Equal(t, OriginalDownloadPath, parsed.Path)
				return address
			}
			get := func(address string, status int) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				require.Equal(t, status, recorder.Code, recorder.Body.String()[:min(100, recorder.Body.Len())])
			}
			address := call()
			for _, method := range []string{http.MethodHead, http.MethodPost} {
				request := httptest.NewRequestWithContext(t.Context(), method, address, nil)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				require.Equal(t, http.StatusMethodNotAllowed, response.Code)
				require.Equal(t, http.MethodGet, response.Header().Get("Allow"))
			}
			get(address, http.StatusOK)
			get(address, http.StatusNotFound)
			get("http://docbank:7341/mcp/original?ticket="+strings.Repeat("A", 43), http.StatusNotFound)
			address = call()
			// Forwarded headers cannot select the origin, and forbidden Host cannot spend a ticket.
			originRequest := httptest.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
			originRequest.Header.Set("Origin", "http://foreign.test:7341")
			originResponse := httptest.NewRecorder()
			handler.ServeHTTP(originResponse, originRequest)
			require.Equal(t, http.StatusForbidden, originResponse.Code)
			forbidden := strings.Replace(address, "docbank:7341", "foreign.test:7341", 1)
			get(forbidden, http.StatusForbidden)
			get(address, http.StatusOK)
		})
	}

	changed := maps.Clone(args)
	changed["blob_hash"] = strings.Repeat("a", 64)
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "get_original_download_url", "arguments": changed}))
	result = decodeResult(t, raw)
	require.Equal(t, true, result["isError"])
	require.Equal(t, "download_selection_stale", objectField(t, result, "structuredContent")["code"])
}

func TestMCPOriginalDownloadLostTicketResponseCanRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("upstream response writer must support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"url":"/api/daemon/web-download/file?ticket=synthetic","name":"original","expires_in_seconds":37}`)
	}))
	t.Cleanup(upstream.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, upstream.URL))
	args := map[string]any{"vault_id": "11111111-1111-4111-8111-111111111111", "node_id": 1,
		"content_version_id": "11111111-1111-4111-8111-111111111111", "blob_hash": strings.Repeat("a", 64), "size": 1}
	result := exportCall(t, server, "get_original_download_url", args)
	require.Equal(t, "download_url_unavailable", result["code"])
	require.Contains(t, result["message"], "request a fresh URL")
	require.EqualValues(t, 1, calls.Load())
	result = exportCall(t, server, "get_original_download_url", args)
	require.Contains(t, result["url"], "ticket=synthetic")
	require.EqualValues(t, 37, result["expires_in_seconds"])
	require.EqualValues(t, 2, calls.Load())
}

type heldDownloadWriter struct {
	*httptest.ResponseRecorder

	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *heldDownloadWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { w.started <- struct{}{}; <-w.release })
	n, err := w.ResponseRecorder.Write(data)
	if err != nil {
		return n, fmt.Errorf("recording download: %w", err)
	}
	return n, nil
}

func TestMCPOriginalDownloadConcurrentToolCall(t *testing.T) {
	server, _, args, handler := suppliedDownloadFixture(t, 8192)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	queries := make([]string, 32)
	for i := range queries {
		queries[i] = issuedDownloadTicket(t, server, args)
	}
	started := make(chan struct{}, 32)
	releases := make([]chan struct{}, 32)
	var done sync.WaitGroup
	for i := range 32 {
		releases[i] = make(chan struct{})
		done.Add(1)
		go func(i int) {
			defer done.Done()
			w := &heldDownloadWriter{ResponseRecorder: httptest.NewRecorder(), started: started, release: releases[i]}
			handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1"+OriginalDownloadPath+"?"+queries[i], nil))
		}(i)
	}
	var releaseOnce sync.Once
	releaseAll := func() {
		releaseOnce.Do(func() {
			for _, release := range releases {
				close(release)
			}
			done.Wait()
		})
	}
	defer releaseAll()
	for range 32 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	raw := requestFor("tools/call", map[string]any{"name": "get_original_download_url", "arguments": args})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(raw))
	req.Header = protocolHeaders("tools/call", "get_original_download_url")
	req.Header.Set("Authorization", "Bearer synthetic-bearer")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotEmpty(t, objectField(t, decodeResult(t, response.Body.Bytes()), "structuredContent")["url"])
	releaseAll()
	query := issuedDownloadTicket(t, server, args)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1"+OriginalDownloadPath+"?"+query, nil))
	require.Equal(t, http.StatusOK, response.Code)
}

func TestMCPOriginalDownloadHTTPSProxy(t *testing.T) {
	f := newNativeExportFixture(t)
	content := strings.Repeat("proxy original", 8192)
	selected := f.addFile(t, "proxy.txt", content)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	var proxyHandler http.Handler
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHandler.ServeHTTP(w, r) }))
	t.Cleanup(proxy.Close)
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer, PublicOrigin: proxy.URL})
	require.NoError(t, err)
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	proxyHandler = httputil.NewSingleHostReverseProxy(target)
	args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID, "content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size}
	for i := range 2 {
		raw := requestFor("tools/call", map[string]any{"name": "get_original_download_url", "arguments": args})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxy.URL+"/mcp", bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header = protocolHeaders("tools/call", "get_original_download_url")
		req.Header.Set("Authorization", "Bearer "+testMCPBearer)
		resp, err := proxy.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		address := conflictField[string](t, objectField(t, decodeResult(t, body), "structuredContent"), "url")
		require.True(t, strings.HasPrefix(address, proxy.URL+OriginalDownloadPath+"?ticket="))
		if i == 1 {
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("DOCBANK_HOME"), "blobs", selected.SHA256[:2], selected.SHA256), bytes.Repeat([]byte{'x'}, len(content)), 0o600))
		}
		req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
		require.NoError(t, err)
		resp, err = proxy.Client().Do(req)
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		if i == 1 {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, resp.Header.Values("Cache-Control"), "no-store")
		require.NotEmpty(t, resp.Trailer.Get("Content-Digest"))
		require.Equal(t, content, string(body))
	}
}

func TestMCPOriginalDownloadRelayRefusesReplacedListener(t *testing.T) {
	f := newNativeExportFixture(t)
	original := "synthetic original bytes"
	selected := f.addFile(t, "original.bin", original)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-review-key"
	root := os.Getenv("DOCBANK_HOME")
	daemon := api.NewServer(api.Deps{Store: f.catalog, Blobs: f.blobs, VaultRoot: root, Cfg: cfg, ShutdownToken: "synthetic-review-proof"})
	t.Cleanup(daemon.Close)
	upstream := httptest.NewServer(daemon.Handler())
	t.Cleanup(upstream.Close)
	address := strings.TrimPrefix(upstream.URL, "http://")
	record := daemonconn.NewRecord(address, cfg.Server.APIKey, "synthetic-review-proof", "")
	_, err := daemonconn.RuntimeStore(root).Write(record)
	require.NoError(t, err)
	connection, running, err := daemonconn.Running(t.Context())
	require.NoError(t, err)
	require.True(t, running)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	server := NewServerWithConnection(connection, ServerOptions{})
	result := exportCall(t, server, "get_original_download_url", map[string]any{
		"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID, "content_version_id": selected.VersionID,
		"blob_hash": selected.SHA256, "size": selected.Size,
	})
	ticketURL, err := url.Parse(conflictField[string](t, result, "url"))
	require.NoError(t, err)
	// A different listener takes the daemon's address. Each download proves ownership
	// afresh, so the relay must refuse it rather than serve its bytes.
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	defaultTransport.CloseIdleConnections()
	require.NoError(t, upstream.Listener.Close())
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
	require.NoError(t, err)
	replacement := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "forged bytes from unproven listener")
	}))
	require.NoError(t, replacement.Listener.Close())
	replacement.Listener = listener
	replacement.Start()
	t.Cleanup(replacement.Close)
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-review-bearer"})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:7341"+OriginalDownloadPath+"?"+ticketURL.RawQuery, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.NotContains(t, response.Body.String(), "forged", "an unproven listener must never supply bytes")
	require.NotContains(t, response.Body.String(), original)
}

func TestMCPOriginalDownloadRelayLogsDownloadFailure(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, upstream.URL))
	var logs bytes.Buffer
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-review-bearer", Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:7341"+OriginalDownloadPath+"?ticket="+strings.Repeat("A", 43), nil)
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Contains(t, logs.String(), `"error_code":"not_found"`)
}

// The callback runs while ReverseProxy still owns the unfinished response body.
type originalDownloadToolWriter struct {
	*httptest.ResponseRecorder

	once sync.Once
	call func()
}

func (w *originalDownloadToolWriter) Write(data []byte) (int, error) {
	w.once.Do(w.call)
	written, err := w.ResponseRecorder.Write(data)
	if err != nil {
		return written, fmt.Errorf("writing recorded response: %w", err)
	}
	return written, nil
}

func suppliedDownloadFixture(t *testing.T, size int) (*Server, *daemonconn.Connection, map[string]any, http.Handler) {
	t.Helper()
	f := newNativeExportFixture(t)
	selected := f.addFile(t, "synthetic.bin", strings.Repeat("s", size))
	connection := f.provenConnection(t)
	server := NewServerWithConnection(connection, ServerOptions{})
	args := map[string]any{"vault_id": f.catalog.VaultID(), "node_id": selected.NodeID,
		"content_version_id": selected.VersionID, "blob_hash": selected.SHA256, "size": selected.Size}
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-bearer"})
	require.NoError(t, err)
	return server, connection, args, handler
}

func issuedDownloadTicket(t *testing.T, server *Server, args map[string]any) string {
	t.Helper()
	issued := exportCall(t, server, "get_original_download_url", args)
	address, err := url.Parse(conflictField[string](t, issued, "url"))
	require.NoError(t, err)
	return address.RawQuery
}

func TestMCPOriginalDownloadSuppliedConnection(t *testing.T) {
	server, connection, args, handler := suppliedDownloadFixture(t, 4<<20)
	query := issuedDownloadTicket(t, server, args)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &originalDownloadToolWriter{ResponseRecorder: httptest.NewRecorder(), call: cancel}
	handler.ServeHTTP(writer, httptest.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1"+OriginalDownloadPath+"?"+query, nil))
	require.NotEmpty(t, exportCall(t, server, "get_original_download_url", args)["url"])
	_, err := connection.API().VaultIdentity(t.Context())
	require.NoError(t, err)
}

func TestMCPOriginalDownloadRelayChecksFreshConnectionCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a rejected credential must not spend the ticket")
	}))
	t.Cleanup(upstream.Close)
	lease := exportTestLease(t, upstream.URL)
	var closed atomic.Int32
	lease.close = func(c *daemonconn.Connection) error { closed.Add(1); return c.Close() }
	require.NoError(t, lease.bindAPIKeyExclusion(daemonconn.NewAPIKeyExclusionPolicy("synthetic-export-key")))
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	ticket := strings.Repeat("A", 43)
	response := httptest.NewRecorder()
	server.relayOriginalDownload(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://127.0.0.1"+OriginalDownloadPath+"?ticket="+ticket, nil))
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.NotContains(t, response.Body.String(), ticket)
	require.EqualValues(t, 1, closed.Load())
}
