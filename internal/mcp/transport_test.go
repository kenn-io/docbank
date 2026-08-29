package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/client"
)

const testMCPBearer = "synthetic-mcp-http-token"

func TestStdioUsesExactlyOneBoundedJSONRPCMessagePerLine(t *testing.T) {
	var compact bytes.Buffer
	require.NoError(t, json.Compact(&compact, fixture(t, "discover.json")))
	request := compact.Bytes()
	input, clientWriter := io.Pipe()
	clientReader, output := io.Pipe()
	var diagnostics bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ServeStdio(t.Context(), newTestServer(), input, output,
			slog.New(slog.NewTextHandler(&diagnostics, nil)))
	}()
	_, err := clientWriter.Write(append(append([]byte{}, request...), '\n'))
	require.NoError(t, err)
	frame, err := bufio.NewReader(clientReader).ReadBytes('\n')
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(frame, []byte{'\n'}),
		"stdout must contain one newline-terminated frame")
	require.NoError(t, clientWriter.Close())
	require.NoError(t, <-done)
	require.NoError(t, clientReader.Close())
	require.NoError(t, output.Close())
	var response map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSuffix(frame, []byte{'\n'}), &response))
	assert.Contains(t, response, "result")
	assert.Empty(t, diagnostics.String())
}

func TestStdioRejectsOversizedFrameBeforeDecodeWithoutEchoingIt(t *testing.T) {
	const sensitive = "synthetic-sensitive-document-text"
	frame := strings.Repeat("x", maxStdioRequestBytes+1) + sensitive + "\n"
	var stdout bytes.Buffer
	var diagnostics bytes.Buffer

	err := ServeStdio(t.Context(), newTestServer(), io.NopCloser(strings.NewReader(frame)),
		&stdout, slog.New(slog.NewTextHandler(&diagnostics, nil)))
	require.ErrorIs(t, err, errInvalidStdioFrame)
	assert.Empty(t, stdout.String())
	assert.NotContains(t, err.Error(), sensitive)
	assert.NotContains(t, diagnostics.String(), sensitive)
	assert.Contains(t, diagnostics.String(), "invalid_frame")
}

func TestStdioPayloadLimitExcludesLFAndCRLFFraming(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(ending, "\r", "CR"), func(t *testing.T) {
			payload := exactStdioJSONPayload(t, maxStdioRequestBytes)
			connection := &stdioConnection{
				input:  io.NopCloser(strings.NewReader(payload + ending)),
				reader: bufio.NewReaderSize(strings.NewReader(payload+ending), maxStdioRequestBytes+2),
			}

			message, err := connection.Read(t.Context())

			require.NoError(t, err)
			assert.NotNil(t, message)
		})
	}
}

func TestStdioRejectsPayloadOneByteOverLimitWithEitherLineEnding(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(ending, "\r", "CR"), func(t *testing.T) {
			payload := exactStdioJSONPayload(t, maxStdioRequestBytes+1)
			connection := &stdioConnection{
				input:  io.NopCloser(strings.NewReader(payload + ending)),
				reader: bufio.NewReaderSize(strings.NewReader(payload+ending), maxStdioRequestBytes+2),
			}

			_, err := connection.Read(t.Context())

			require.ErrorIs(t, err, errInvalidStdioFrame)
		})
	}
}

func exactStdioJSONPayload(t *testing.T, size int) string {
	t.Helper()
	const prefix = `{"jsonrpc":"2.0","id":1,"method":"synthetic","params":{"padding":"`
	const suffix = `"}}`
	require.GreaterOrEqual(t, size, len(prefix)+len(suffix))
	return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
}

func TestStdioRejectsEmbeddedNewlineAsSeparateInvalidFrame(t *testing.T) {
	const sensitive = "synthetic-secret-in-second-line"
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\n\"method\":\"" + sensitive + "\"}\n"
	var stdout bytes.Buffer
	var diagnostics bytes.Buffer

	err := ServeStdio(context.Background(), newTestServer(), io.NopCloser(strings.NewReader(input)),
		&stdout, slog.New(slog.NewTextHandler(&diagnostics, nil)))
	require.ErrorIs(t, err, errInvalidStdioFrame)
	assert.Empty(t, stdout.String())
	assert.NotContains(t, err.Error(), sensitive)
	assert.NotContains(t, diagnostics.String(), sensitive)
}

type blockingTestParams struct{ sdkmcp.ParamsBase }
type blockingTestResult struct{ sdkmcp.ResultBase }

func TestStdioCancelledNotificationCancelsTheMatchingCall(t *testing.T) {
	server := newTestServer()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	require.NoError(t, sdkmcp.AddReceivingCustomMethod(server.sdk, "synthetic/block",
		func(ctx context.Context, _ *sdkmcp.ServerSession, _ *blockingTestParams) (*blockingTestResult, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}))
	input, clientWriter := io.Pipe()
	clientReader, output := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- ServeStdio(t.Context(), server, input, output, slog.New(slog.DiscardHandler))
	}()

	writeStdioJSON(t, clientWriter, map[string]any{
		"jsonrpc": "2.0", "id": 73, "method": "synthetic/block",
		"params": map[string]any{"_meta": testRequestMeta()},
	})
	<-started
	writeStdioJSON(t, clientWriter, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 73, "reason": "caller stopped", "_meta": testRequestMeta()},
	})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("notifications/cancelled did not cancel the matching stdio call")
	}
	_, err := bufio.NewReader(clientReader).ReadBytes('\n')
	require.NoError(t, err)
	require.NoError(t, clientWriter.Close())
	require.NoError(t, <-done)
	require.NoError(t, clientReader.Close())
	require.NoError(t, output.Close())
}

func writeStdioJSON(t *testing.T, writer io.Writer, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	_, err = writer.Write(encoded)
	require.NoError(t, err)
}

func testRequestMeta() map[string]any {
	return map[string]any{
		sdkmcp.MetaKeyProtocolVersion:    ProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{},
	}
}

func TestHTTPListenAddressMustBeAnExplicitLoopbackIPAndPort(t *testing.T) {
	tests := []struct {
		address string
		valid   bool
	}{
		{address: "127.0.0.1:7341", valid: true},
		{address: "[::1]:7341", valid: true},
		{address: "127.0.0.1:0", valid: true},
		{address: "[::1%loopback]:7341"},
		{address: "localhost:7341"},
		{address: "0.0.0.0:7341"},
		{address: "192.0.2.4:7341"},
		{address: "127.0.0.1"},
		{address: ":7341"},
		{address: "127.0.0.1:70000"},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			err := ValidateHTTPListenAddress(test.address)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), testMCPBearer)
			}
		})
	}
}

func TestHTTPTransportRequiresBearerAndRejectsUnsafeOriginBeforeAuth(t *testing.T) {
	handler, err := newTestServer().HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)

	tests := []struct {
		name       string
		origin     string
		auth       string
		wantStatus int
	}{
		{name: "non-browser authenticated", auth: "Bearer " + testMCPBearer, wantStatus: http.StatusOK},
		{name: "same origin authenticated", origin: "http://127.0.0.1", auth: "Bearer " + testMCPBearer, wantStatus: http.StatusOK},
		{name: "missing bearer", wantStatus: http.StatusUnauthorized},
		{name: "wrong bearer", auth: "Bearer daemon-api-key", wantStatus: http.StatusUnauthorized},
		{name: "unsafe origin precedes missing auth", origin: "https://attacker.example", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newHTTPDiscoverRequest(t)
			request.Header.Set("Authorization", test.auth)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, test.wantStatus, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.NotContains(t, response.Body.String(), testMCPBearer)
			assert.NotContains(t, response.Body.String(), "daemon-api-key")
			assert.Empty(t, response.Header().Get("Mcp-Session-Id"))
		})
	}
}

func TestHTTPTransportValidatesHostAndOriginIndependentlyBeforeAuth(t *testing.T) {
	handler, err := newTestServer().HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)

	tests := []struct {
		name, host, origin string
		wantStatus         int
	}{
		{name: "absent origin", host: "127.0.0.1", wantStatus: http.StatusUnauthorized},
		{name: "localhost", host: "localhost:7341", origin: "http://localhost:7341", wantStatus: http.StatusUnauthorized},
		{name: "IPv4 127 slash 8", host: "127.42.0.8:7341", origin: "http://127.42.0.8:7341", wantStatus: http.StatusUnauthorized},
		{name: "IPv6", host: "[::1]:7341", origin: "http://[::1]:7341", wantStatus: http.StatusUnauthorized},
		{name: "canonical IPv6", host: "[0:0:0:0:0:0:0:1]:7341", origin: "http://[::1]:7341", wantStatus: http.StatusUnauthorized},
		{name: "mapped IPv6", host: "[::ffff:127.0.0.1]:7341", origin: "http://[::ffff:7f00:1]:7341", wantStatus: http.StatusUnauthorized},
		{name: "implicit default port", host: "127.0.0.1", origin: "http://127.0.0.1:80", wantStatus: http.StatusUnauthorized},
		{name: "attacker matching", host: "attacker.example", origin: "http://attacker.example", wantStatus: http.StatusForbidden},
		{name: "non-loopback IPv4 matching", host: "192.0.2.4:7341", origin: "http://192.0.2.4:7341", wantStatus: http.StatusForbidden},
		{name: "malformed host", host: "127.0.0.1:not-a-port", wantStatus: http.StatusForbidden},
		{name: "empty port", host: "localhost:", wantStatus: http.StatusForbidden},
		{name: "IPv6 zone", host: "[::1%loopback]:7341", wantStatus: http.StatusForbidden},
		{name: "different local host", host: "localhost:7341", origin: "http://127.0.0.1:7341", wantStatus: http.StatusForbidden},
		{name: "mapped host differs from IPv4 origin", host: "[::ffff:127.0.0.1]:7341", origin: "http://127.0.0.1:7341", wantStatus: http.StatusForbidden},
		{name: "IPv4 host differs from mapped origin", host: "127.0.0.1:7341", origin: "http://[::ffff:127.0.0.1]:7341", wantStatus: http.StatusForbidden},
		{name: "different port", host: "127.0.0.1:7341", origin: "http://127.0.0.1:7342", wantStatus: http.StatusForbidden},
		{name: "HTTPS", host: "127.0.0.1:7341", origin: "https://127.0.0.1:7341", wantStatus: http.StatusForbidden},
		{name: "origin path", host: "127.0.0.1:7341", origin: "http://127.0.0.1:7341/path", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newHTTPDiscoverRequest(t)
			request.Host = test.host
			request.Header.Del("Authorization")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			// These must never affect validation.
			request.Header.Set("Forwarded", "host=127.0.0.1:7341;proto=http")
			request.Header.Set("X-Forwarded-Host", "127.0.0.1:7341")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, test.wantStatus, response.Code)
		})
	}
}

func TestHTTPTransportIsPOSTOnlyAndRejectsSessionOrResumeHeaders(t *testing.T) {
	handler, err := newTestServer().HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			request := newHTTPDiscoverRequest(t)
			request.Method = method
			request.Header.Set("Authorization", "Bearer "+testMCPBearer)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, http.StatusMethodNotAllowed, response.Code)
			assert.Equal(t, "POST", response.Header().Get("Allow"))
		})
	}

	for _, test := range []struct{ header, value string }{
		{header: "Mcp-Session-Id", value: "unsupported-state"},
		{header: "Last-Event-ID", value: "unsupported-state"},
		{header: "Mcp-Session-Id"},
		{header: "Last-Event-ID"},
	} {
		t.Run(test.header+"/"+test.value, func(t *testing.T) {
			request := newHTTPDiscoverRequest(t)
			request.Header.Set("Authorization", "Bearer "+testMCPBearer)
			request.Header.Set(test.header, test.value)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, http.StatusBadRequest, response.Code)
			assert.Empty(t, response.Header().Get("Mcp-Session-Id"))
		})
	}
}

func TestHTTPTransportCanReturnJSONOrSSEWithoutSessions(t *testing.T) {
	for _, test := range []struct {
		name         string
		jsonResponse bool
		contentType  string
		bodyContains string
	}{
		{name: "json", jsonResponse: true, contentType: "application/json", bodyContains: `"result"`},
		{name: "sse", contentType: "text/event-stream", bodyContains: "data: "},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, err := newTestServer().HTTPTransportHandler(HTTPOptions{
				BearerToken: testMCPBearer, JSONResponse: test.jsonResponse,
			})
			require.NoError(t, err)
			request := newHTTPDiscoverRequest(t)
			request.Header.Set("Authorization", "Bearer "+testMCPBearer)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assert.Equal(t, http.StatusOK, response.Code)
			assert.Contains(t, response.Header().Get("Content-Type"), test.contentType)
			assert.Contains(t, response.Body.String(), test.bodyContains)
			assert.Empty(t, response.Header().Get("Mcp-Session-Id"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		})
	}
}

func TestHTTPTransportEnforcesBodyAndHeaderLimitsBeforeDispatch(t *testing.T) {
	var dispatched bool
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dispatched = true })
	handler, err := wrapHTTPTransport(inner, HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)

	bodyRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp",
		strings.NewReader(strings.Repeat("x", maxHTTPRequestBytes+1)))
	bodyRequest.Header = authenticatedTransportHeaders()
	bodyResponse := httptest.NewRecorder()
	handler.ServeHTTP(bodyResponse, bodyRequest)
	assert.Equal(t, http.StatusRequestEntityTooLarge, bodyResponse.Code)
	assert.False(t, dispatched)

	headerRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader("{}"))
	headerRequest.Header = authenticatedTransportHeaders()
	headerRequest.Header.Set("X-Synthetic-Oversized", strings.Repeat("h", maxHTTPHeaderBytes))
	headerResponse := httptest.NewRecorder()
	handler.ServeHTTP(headerResponse, headerRequest)
	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, headerResponse.Code)
	assert.False(t, dispatched)
}

func TestHTTPTransportRejectsChunkedOversizedBodyBeforeProtocolDecode(t *testing.T) {
	handler, err := newTestServer().HTTPTransportHandler(HTTPOptions{
		BearerToken: testMCPBearer, JSONResponse: true,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp",
		strings.NewReader(strings.Repeat("x", maxHTTPRequestBytes+1)))
	request.ContentLength = -1
	request.Header = protocolHeaders("server/discover", "")
	request.Header.Set("Authorization", "Bearer "+testMCPBearer)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
}

func TestHTTPTransportBoundsGlobalAndPerClientActivity(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	inner := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		response.WriteHeader(http.StatusNoContent)
	})
	handler, err := wrapHTTPTransport(inner, HTTPOptions{
		BearerToken: testMCPBearer,
		Limits:      HTTPLimits{MaxConcurrentRequests: 2, MaxConcurrentPerClient: 1},
	})
	require.NoError(t, err)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedRequest("127.0.0.1:41001"))
		firstDone <- response
	}()
	<-entered

	sameClient := httptest.NewRecorder()
	handler.ServeHTTP(sameClient, authenticatedRequest("127.0.0.1:41002"))
	assert.Equal(t, http.StatusTooManyRequests, sameClient.Code)

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedRequest("127.0.0.2:41001"))
		secondDone <- response
	}()
	<-entered

	global := httptest.NewRecorder()
	handler.ServeHTTP(global, authenticatedRequest("127.0.0.3:41001"))
	assert.Equal(t, http.StatusTooManyRequests, global.Code)

	close(release)
	assert.Equal(t, http.StatusNoContent, (<-firstDone).Code)
	assert.Equal(t, http.StatusNoContent, (<-secondDone).Code)
}

func TestHTTPTransportBuffersJSONResponseBeforeEnforcingExactCap(t *testing.T) {
	const limit = 64
	for _, test := range []struct {
		name       string
		bodyBytes  int
		wantStatus int
		wantBody   string
	}{
		{name: "exact cap", bodyBytes: limit, wantStatus: http.StatusOK, wantBody: strings.Repeat("x", limit)},
		{name: "cap plus one", bodyBytes: limit + 1, wantStatus: http.StatusInternalServerError,
			wantBody: "MCP response too large\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusOK)
				_, _ = response.Write([]byte(strings.Repeat("x", test.bodyBytes)))
			})
			handler, err := wrapHTTPTransport(inner, HTTPOptions{
				BearerToken: testMCPBearer, JSONResponse: true,
				Limits: HTTPLimits{MaxResponseBytes: limit, MaxConcurrentRequests: 1,
					MaxConcurrentPerClient: 1, RequestTimeout: time.Second},
			})
			require.NoError(t, err)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, authenticatedRequest("127.0.0.1:41001"))

			assert.Equal(t, test.wantStatus, response.Code)
			assert.Equal(t, test.wantBody, response.Body.String())
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		})
	}
}

func TestHTTPTransportSSECapEmitsOnlyCompleteEvents(t *testing.T) {
	const limit = 32
	first := "event: first\ndata: accepted\n\n"
	require.Less(t, len(first), limit)
	tests := []struct {
		name, writes, want string
	}{
		{name: "exact cap", writes: "data: " + strings.Repeat("x", limit-len("data: \n\n")) + "\n\n",
			want: "data: " + strings.Repeat("x", limit-len("data: \n\n")) + "\n\n"},
		{name: "cap plus one event", writes: "data: " + strings.Repeat("x", limit+1-len("data: \n\n")) + "\n\n"},
		{name: "complete then partial oversized event", writes: first + strings.Repeat("p", limit) + "\n\n", want: first},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				response.WriteHeader(http.StatusOK)
				for _, part := range []string{test.writes[:len(test.writes)/2], test.writes[len(test.writes)/2:]} {
					_, _ = response.Write([]byte(part))
					if flusher, ok := response.(http.Flusher); ok {
						flusher.Flush()
					}
				}
			})
			handler, err := wrapHTTPTransport(inner, HTTPOptions{BearerToken: testMCPBearer,
				Limits: HTTPLimits{MaxResponseBytes: limit, MaxConcurrentRequests: 1,
					MaxConcurrentPerClient: 1, RequestTimeout: time.Second}})
			require.NoError(t, err)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, authenticatedRequest("127.0.0.1:41001"))

			assert.Equal(t, test.want, response.Body.String())
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		})
	}
}

func TestHTTPTransportCapsResponsesAndLogsOnlyStableTelemetry(t *testing.T) {
	const sensitive = "synthetic-private-path-and-text"
	var logs bytes.Buffer
	inner := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(strings.Repeat(sensitive, 100)))
	})
	handler, err := wrapHTTPTransport(inner, HTTPOptions{
		BearerToken: testMCPBearer,
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
		Limits: HTTPLimits{
			MaxResponseBytes: 128, MaxConcurrentRequests: 1, MaxConcurrentPerClient: 1,
			RequestTimeout: time.Second,
		},
	})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest("127.0.0.1:41001"))

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Equal(t, "MCP response too large\n", response.Body.String())
	assert.Contains(t, logs.String(), "response_too_large")
	assert.Contains(t, logs.String(), "latency_ms")
	assert.Contains(t, logs.String(), "response_bytes")
	assert.NotContains(t, logs.String(), sensitive)
	assert.NotContains(t, logs.String(), testMCPBearer)
}

func TestHTTPTransportSocketDeadlinesInterruptSlowIOAndReleaseActivity(t *testing.T) {
	for _, test := range []struct {
		name       string
		newRequest func(*deadlineResponseWriter) *http.Request
		inner      http.Handler
	}{
		{
			name: "slow upload",
			newRequest: func(writer *deadlineResponseWriter) *http.Request {
				request := authenticatedRequest("127.0.0.1:41001")
				request.Body = &deadlineBody{deadline: writer.readExpired}
				request.ContentLength = -1
				return request
			},
			inner: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				_, _ = io.ReadAll(request.Body)
			}),
		},
		{
			name: "slow response reader",
			newRequest: func(*deadlineResponseWriter) *http.Request {
				return authenticatedRequest("127.0.0.1:41001")
			},
			inner: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = response.Write([]byte("bounded response"))
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, err := wrapHTTPTransport(test.inner, HTTPOptions{BearerToken: testMCPBearer,
				JSONResponse: true, Limits: HTTPLimits{MaxConcurrentRequests: 1,
					MaxConcurrentPerClient: 1, MaxResponseBytes: 64, RequestTimeout: 20 * time.Millisecond}})
			require.NoError(t, err)
			writer := newDeadlineResponseWriter(test.name == "slow response reader")
			done := make(chan struct{})
			go func() {
				handler.ServeHTTP(writer, test.newRequest(writer))
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("socket deadline did not interrupt blocked I/O")
			}

			// The timed-out request must no longer hold either semaphore.
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedRequest("127.0.0.1:41001"))
			assert.NotEqual(t, http.StatusTooManyRequests, response.Code)
			assert.True(t, writer.readDeadlineSet || writer.writeDeadlineSet)
		})
	}
}

type deadlineResponseWriter struct {
	*httptest.ResponseRecorder

	mu               sync.Mutex
	readExpired      chan struct{}
	writeExpired     chan struct{}
	readDeadlineSet  bool
	writeDeadlineSet bool
	blockWrites      bool
	readTimer        *time.Timer
	writeTimer       *time.Timer
}

func newDeadlineResponseWriter(blockWrites bool) *deadlineResponseWriter {
	return &deadlineResponseWriter{ResponseRecorder: httptest.NewRecorder(),
		readExpired: make(chan struct{}), writeExpired: make(chan struct{}), blockWrites: blockWrites}
}

func (writer *deadlineResponseWriter) SetReadDeadline(deadline time.Time) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if deadline.IsZero() {
		return nil
	}
	writer.readDeadlineSet = true
	writer.readTimer = time.AfterFunc(time.Until(deadline), func() { close(writer.readExpired) })
	return nil
}

func (writer *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if deadline.IsZero() {
		return nil
	}
	writer.writeDeadlineSet = true
	writer.writeTimer = time.AfterFunc(time.Until(deadline), func() { close(writer.writeExpired) })
	return nil
}

func (writer *deadlineResponseWriter) Write(body []byte) (int, error) {
	if writer.blockWrites {
		<-writer.writeExpired
		return 0, timeoutTestError{}
	}
	written, err := writer.ResponseRecorder.Write(body)
	if err != nil {
		return written, fmt.Errorf("recording synthetic deadline response: %w", err)
	}
	return written, nil
}

type deadlineBody struct{ deadline <-chan struct{} }

func (body *deadlineBody) Read([]byte) (int, error) {
	<-body.deadline
	return 0, timeoutTestError{}
}

func (*deadlineBody) Close() error { return nil }

type timeoutTestError struct{}

func (timeoutTestError) Error() string   { return "synthetic timeout" }
func (timeoutTestError) Timeout() bool   { return true }
func (timeoutTestError) Temporary() bool { return true }

func TestHTTPStartupAcquiresDaemonAndRejectsItsEffectiveRuntimeKey(t *testing.T) {
	const runtimeKey = "synthetic-effective-runtime-key"
	for _, test := range []struct {
		name, bearer string
		wantError    bool
	}{
		{name: "ephemeral empty-config key reused", bearer: runtimeKey, wantError: true},
		{name: "already-running runtime has different key", bearer: "independent-mcp-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var acquired int
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				acquired++
				return client.New("http://127.0.0.1:1", runtimeKey), nil
			}, func(*client.Client) error { return nil })
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

			err := server.prepareHTTP(t.Context(), test.bearer)

			assert.Equal(t, 1, acquired)
			if test.wantError {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), runtimeKey)
				assert.NotContains(t, err.Error(), test.bearer)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestHTTPStartupFailsClosedWhenDaemonCannotBeEstablished(t *testing.T) {
	const sensitive = "synthetic-daemon-secret"
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return nil, errors.New("daemon failed with " + sensitive)
	}, func(*client.Client) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)

	err := server.prepareHTTP(t.Context(), testMCPBearer)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), sensitive)
	assert.NotContains(t, err.Error(), testMCPBearer)
}

func TestHTTPSSEClosePropagatesCancellationIntoSDKHandler(t *testing.T) {
	server := newTestServer()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	require.NoError(t, sdkmcp.AddReceivingCustomMethod(server.sdk, "synthetic/block",
		func(ctx context.Context, _ *sdkmcp.ServerSession, _ *blockingTestParams) (*blockingTestResult, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}))
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 91, "method": "synthetic/block",
		"params": map[string]any{"_meta": testRequestMeta()},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(body)).WithContext(ctx)
	request.Header = protocolHeaders("synthetic/block", "")
	request.Header.Set("Authorization", "Bearer "+testMCPBearer)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("closing the HTTP SSE request did not cancel the SDK handler")
	}
	<-done
}

func newHTTPDiscoverRequest(t *testing.T) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp",
		bytes.NewReader(fixture(t, "discover.json")))
	request.Header = protocolHeaders("server/discover", "")
	return request
}

func authenticatedTransportHeaders() http.Header {
	header := transportHeaders()
	header.Set("Authorization", "Bearer "+testMCPBearer)
	return header
}

func authenticatedRequest(remoteAddress string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader("{}"))
	request.Header = authenticatedTransportHeaders()
	request.RemoteAddr = remoteAddress
	return request
}
