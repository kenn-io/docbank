package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/version"
)

func TestServerDiscoverPublishesOnlyDocbankExactProtocol(t *testing.T) {
	response := exchangeRaw(t, newTestServer(), fixture(t, "discover.json"))

	var wire struct {
		Result struct {
			ResultType        string                     `json:"resultType"`
			Meta              map[string]json.RawMessage `json:"_meta"`
			TTLMs             int                        `json:"ttlMs"`
			CacheScope        string                     `json:"cacheScope"`
			SupportedVersions []string                   `json:"supportedVersions"`
			Capabilities      map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &wire))
	assert.Equal(t, "complete", wire.Result.ResultType)
	assert.Positive(t, wire.Result.TTLMs)
	assert.Equal(t, "public", wire.Result.CacheScope)
	assert.Equal(t, []string{ProtocolVersion}, wire.Result.SupportedVersions)
	assert.JSONEq(t, `{}`, string(wire.Result.Capabilities["tools"]))
	assert.JSONEq(t, `{}`, string(wire.Result.Capabilities["resources"]))
	for _, absent := range []string{"prompts", "roots", "sampling", "elicitation", "tasks", "progress"} {
		assert.NotContains(t, wire.Result.Capabilities, absent)
	}

	var serverInfo sdkmcp.Implementation
	require.NoError(t, json.Unmarshal(wire.Result.Meta[sdkmcp.MetaKeyServerInfo], &serverInfo))
	assert.Equal(t, sdkmcp.Implementation{
		Name:        "docbank",
		Title:       "Docbank",
		Description: "Self-sovereign document system",
		Version:     version.Version,
	}, serverInfo)
}

func TestIngressRejectsRequestsOutsideExactProtocolBeforeSDKDispatch(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		requested string
	}{
		{name: "missing metadata", fixture: "missing-metadata.json"},
		{name: "wrong version", fixture: "wrong-version.json", requested: "2025-11-25"},
		{name: "legacy initialize", fixture: "initialize.json", requested: "2025-11-25"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer()
			var dispatched atomic.Int64
			server.sdk.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
				return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
					dispatched.Add(1)
					return next(ctx, method, request)
				}
			})

			response := exchangeRaw(t, server, fixture(t, test.fixture))
			wireErr := decodeWireError(t, response)
			assert.Equal(t, int64(sdkmcp.CodeUnsupportedProtocolVersion), wireErr.Code)
			assert.Contains(t, wireErr.Message, ProtocolVersion)
			assert.Equal(t, int64(0), dispatched.Load(), "rejected request reached SDK middleware")

			var data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			}
			require.NoError(t, json.Unmarshal(wireErr.Data, &data))
			assert.Equal(t, []string{ProtocolVersion}, data.Supported)
			assert.Equal(t, test.requested, data.Requested)
		})
	}
}

func TestIngressRejectsMissingClientCapabilities(t *testing.T) {
	wireErr := decodeWireError(t, exchangeRaw(t, newTestServer(), fixture(t, "missing-client-capabilities.json")))
	assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
	assert.Contains(t, wireErr.Message, sdkmcp.MetaKeyClientCapabilities)
}

func TestIngressLeavesUnknownCurrentProtocolMethodForSDK(t *testing.T) {
	wireErr := decodeWireError(t, exchangeRaw(t, newTestServer(), fixture(t, "unknown-method.json")))
	assert.Equal(t, int64(jsonrpc.CodeMethodNotFound), wireErr.Code)
}

func TestIngressBlocksEveryLegacyVersionAdvertisedByPinnedSDK(t *testing.T) {
	bare := sdkmcp.NewServer(testImplementation(), &sdkmcp.ServerOptions{
		Capabilities: &sdkmcp.ServerCapabilities{},
	})
	response := exchangeRunnerRaw(t, bare, fixture(t, "discover.json"))
	var discovery struct {
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response, &discovery))
	require.Greater(t, len(discovery.Result.SupportedVersions), 1,
		"guard requires the pinned SDK to advertise a broader compatibility set")

	server := newTestServer()
	var dispatched atomic.Int64
	server.sdk.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			dispatched.Add(1)
			return next(ctx, method, request)
		}
	})
	for _, version := range discovery.Result.SupportedVersions {
		if version == ProtocolVersion {
			continue
		}
		t.Run(version, func(t *testing.T) {
			request := requestWithVersion(t, fixture(t, "discover.json"), version)
			wireErr := decodeWireError(t, exchangeRaw(t, server, request))
			assert.Equal(t, int64(sdkmcp.CodeUnsupportedProtocolVersion), wireErr.Code)
		})
	}
	assert.Equal(t, int64(0), dispatched.Load(), "an SDK-compatible legacy version reached dispatch")
}

func TestHTTPIngressEnforcesHeadersAndAcceptBeforeSDKDispatch(t *testing.T) {
	tests := []struct {
		name        string
		fixture     string
		headers     http.Header
		wantStatus  int
		wantCode    int64
		wantMessage string
	}{
		{
			name:       "valid discovery",
			fixture:    "discover.json",
			headers:    protocolHeaders("server/discover", ""),
			wantStatus: http.StatusOK,
		},
		{
			name:       "combined JSON and SSE accept",
			fixture:    "discover.json",
			headers:    headersWith(protocolHeaders("server/discover", ""), "Accept", "application/json, text/event-stream"),
			wantStatus: http.StatusOK,
		},
		{
			name:        "missing protocol version",
			fixture:     "discover.json",
			headers:     headersWithout(protocolHeaders("server/discover", ""), "Mcp-Protocol-Version"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    sdkmcp.CodeHeaderMismatch,
			wantMessage: "Mcp-Protocol-Version",
		},
		{
			name:        "mismatched protocol version",
			fixture:     "discover.json",
			headers:     headersWith(protocolHeaders("server/discover", ""), "Mcp-Protocol-Version", "2025-11-25"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    sdkmcp.CodeHeaderMismatch,
			wantMessage: "Mcp-Protocol-Version",
		},
		{
			name:        "missing method",
			fixture:     "discover.json",
			headers:     headersWithout(protocolHeaders("server/discover", ""), "Mcp-Method"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    sdkmcp.CodeHeaderMismatch,
			wantMessage: "Mcp-Method",
		},
		{
			name:        "mismatched method",
			fixture:     "discover.json",
			headers:     headersWith(protocolHeaders("server/discover", ""), "Mcp-Method", "tools/list"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    sdkmcp.CodeHeaderMismatch,
			wantMessage: "Mcp-Method",
		},
		{
			name:        "missing required name",
			fixture:     "read-resource.json",
			headers:     protocolHeaders("resources/read", ""),
			wantStatus:  http.StatusBadRequest,
			wantCode:    sdkmcp.CodeHeaderMismatch,
			wantMessage: "Mcp-Name",
		},
		{
			name:        "mismatched required name",
			fixture:     "read-resource.json",
			headers:     protocolHeaders("resources/read", "docbank://synthetic/other"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    sdkmcp.CodeHeaderMismatch,
			wantMessage: "Mcp-Name",
		},
		{
			name:        "JSON alone is insufficient",
			fixture:     "discover.json",
			headers:     headersWith(protocolHeaders("server/discover", ""), "Accept", "application/json"),
			wantStatus:  http.StatusBadRequest,
			wantMessage: "both 'application/json' and 'text/event-stream'",
		},
		{
			name:        "SSE alone is insufficient",
			fixture:     "discover.json",
			headers:     headersWith(protocolHeaders("server/discover", ""), "Accept", "text/event-stream"),
			wantStatus:  http.StatusBadRequest,
			wantMessage: "both 'application/json' and 'text/event-stream'",
		},
	}

	handler := newTestServer().HTTPHandler()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(fixture(t, test.fixture)))
			request.Header = test.headers.Clone()
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			if test.wantCode != 0 {
				wireErr := decodeWireError(t, recorder.Body.Bytes())
				assert.Equal(t, test.wantCode, wireErr.Code)
				assert.Contains(t, wireErr.Message, test.wantMessage)
			} else if test.wantMessage != "" {
				assert.Contains(t, recorder.Body.String(), test.wantMessage)
			}
		})
	}
}

func TestHTTPUnknownMethodReturnsNotFoundJSONRPCError(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(fixture(t, "unknown-method.json")))
	request.Header = protocolHeaders("docbank/unknown", "")
	recorder := httptest.NewRecorder()

	newTestServer().HTTPHandler().ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	assert.Equal(t, int64(jsonrpc.CodeMethodNotFound), decodeWireError(t, recorder.Body.Bytes()).Code)
}

func TestHTTPIngressRejectsInvalidProtocolBeforeHeaderMirroring(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		headers   http.Header
		requested string
	}{
		{
			name:      "legacy initialize without new protocol headers",
			fixture:   "initialize.json",
			headers:   transportHeaders(),
			requested: "2025-11-25",
		},
		{
			name:      "missing request metadata",
			fixture:   "missing-metadata.json",
			headers:   protocolHeaders("server/discover", ""),
			requested: "",
		},
		{
			name:      "wrong version with matching headers",
			fixture:   "wrong-version.json",
			headers:   headersWith(protocolHeaders("server/discover", ""), "Mcp-Protocol-Version", "2025-11-25"),
			requested: "2025-11-25",
		},
	}

	handler := newTestServer().HTTPHandler()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(fixture(t, test.fixture)))
			request.Header = test.headers.Clone()
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			wireErr := decodeWireError(t, recorder.Body.Bytes())
			assert.Equal(t, int64(sdkmcp.CodeUnsupportedProtocolVersion), wireErr.Code)
			assert.Contains(t, wireErr.Message, ProtocolVersion)
			var data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			}
			require.NoError(t, json.Unmarshal(wireErr.Data, &data))
			assert.Equal(t, []string{ProtocolVersion}, data.Supported)
			assert.Equal(t, test.requested, data.Requested)
		})
	}
}

func TestHTTPHeaderMismatchStopsBeforeSDKDispatch(t *testing.T) {
	server := newTestServer()
	var dispatched atomic.Int64
	server.sdk.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			dispatched.Add(1)
			return next(ctx, method, request)
		}
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(fixture(t, "discover.json")))
	request.Header = headersWithout(protocolHeaders("server/discover", ""), "Mcp-Method")
	recorder := httptest.NewRecorder()

	server.HTTPHandler().ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Equal(t, int64(0), dispatched.Load())
}

type runner interface {
	Run(ctx context.Context, transport sdkmcp.Transport) error
}

func exchangeRaw(t *testing.T, server *Server, request []byte) []byte {
	t.Helper()
	return exchangeRunnerRaw(t, server, request)
}

func exchangeRunnerRaw(t *testing.T, server runner, request []byte) []byte {
	t.Helper()
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, &sdkmcp.IOTransport{Reader: serverReader, Writer: serverWriter})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientWriter.Close()
		_ = clientReader.Close()
		select {
		case <-done:
		default:
		}
	})

	compact := new(bytes.Buffer)
	require.NoError(t, json.Compact(compact, request))
	compact.WriteByte('\n')
	_, err := clientWriter.Write(compact.Bytes())
	require.NoError(t, err)
	response, err := bufio.NewReader(clientReader).ReadBytes('\n')
	require.NoError(t, err)
	return bytes.TrimSpace(response)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return data
}

func requestWithVersion(t *testing.T, request []byte, version string) []byte {
	t.Helper()
	var wire map[string]any
	require.NoError(t, json.Unmarshal(request, &wire))
	params, ok := wire["params"].(map[string]any)
	require.True(t, ok)
	meta, ok := params["_meta"].(map[string]any)
	require.True(t, ok)
	meta[sdkmcp.MetaKeyProtocolVersion] = version
	result, err := json.Marshal(wire)
	require.NoError(t, err)
	return result
}

func decodeWireError(t *testing.T, response []byte) *jsonrpc.Error {
	t.Helper()
	var wire struct {
		Error *jsonrpc.Error `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response, &wire))
	require.NotNil(t, wire.Error, "response did not contain a JSON-RPC error: %s", response)
	return wire.Error
}

func testImplementation() *sdkmcp.Implementation {
	return &sdkmcp.Implementation{
		Name:        "docbank",
		Title:       "Docbank",
		Description: "Self-sovereign document system",
		Version:     "test-version",
	}
}

func newTestServer() *Server {
	return NewServer()
}

func protocolHeaders(method, name string) http.Header {
	header := transportHeaders()
	header["Mcp-Protocol-Version"] = []string{ProtocolVersion}
	header["Mcp-Method"] = []string{method}
	if name != "" {
		header.Set("Mcp-Name", name)
	}
	return header
}

func transportHeaders() http.Header {
	return http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json", "text/event-stream"},
	}
}

func headersWithout(header http.Header, key string) http.Header {
	clone := header.Clone()
	clone.Del(key)
	return clone
}

func headersWith(header http.Header, key, value string) http.Header {
	clone := header.Clone()
	clone.Set(key, value)
	return clone
}
