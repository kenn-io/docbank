// Package mcp implements Docbank's exact Model Context Protocol boundary.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/version"
)

const (
	// ProtocolVersion is the only MCP protocol version accepted by Docbank.
	ProtocolVersion = "2026-07-28"

	discoveryTTLMs = 60_000
)

// Server owns the Docbank ingress gate and the SDK server behind it.
type Server struct {
	sdk    *sdkmcp.Server
	daemon *daemonLease
	plans  *processingPlanRegistry
}

// ServerOptions fixes process-wide capabilities before the MCP server starts.
// The catalog never changes during the lifetime of a server.
type ServerOptions struct {
	AllowProcessing bool
}

// NewServer creates an exact-version Docbank MCP server.
func NewServer() *Server {
	return NewServerWithOptions(ServerOptions{})
}

// NewServerWithOptions creates an exact-version server with a process-fixed
// catalog. Processing remains absent unless explicitly enabled here.
func NewServerWithOptions(options ServerOptions) *Server {
	return newServerWithOptions(&sdkmcp.Implementation{
		Name:        "docbank",
		Title:       "Docbank",
		Description: "Self-sovereign document system",
		Version:     version.Version,
	}, options)
}

func newServerWithOptions(implementation *sdkmcp.Implementation, options ServerOptions) *Server {
	return newServerWithOptionsAndDaemon(implementation, options, newDaemonLease())
}

func newServerWithOptionsAndDaemon(
	implementation *sdkmcp.Implementation, options ServerOptions, daemon *daemonLease,
) *Server {
	if implementation == nil {
		panic("nil MCP implementation")
	}
	if daemon == nil {
		panic("nil daemon lease")
	}

	sdk := sdkmcp.NewServer(implementation, &sdkmcp.ServerOptions{
		Capabilities: &sdkmcp.ServerCapabilities{
			Resources: &sdkmcp.ResourceCapabilities{},
			Tools:     &sdkmcp.ToolCapabilities{},
		},
		Instructions: catalogInstructions(options.AllowProcessing),
	})
	plans := newProcessingPlanRegistry()
	registerToolCatalog(sdk, options.AllowProcessing, daemon, plans)
	registerResourceSurface(sdk, daemon)
	sdk.AddReceivingMiddleware(normalizeDiscovery)
	sdk.AddReceivingMiddleware(normalizeToolCatalog)
	sdk.AddReceivingMiddleware(normalizeResourceCatalogs)
	sdk.AddReceivingMiddleware(enforcePrivateResultCap(implementation))
	return &Server{sdk: sdk, daemon: daemon, plans: plans}
}

func enforcePrivateResultCap(implementation *sdkmcp.Implementation) sdkmcp.Middleware {
	return func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			result, err := next(ctx, method, request)
			if err != nil || result == nil || (method != "tools/call" && method != "resources/read") {
				return result, err
			}
			metadata := make(map[string]any, len(result.GetMeta())+1)
			maps.Copy(metadata, result.GetMeta())
			if _, exists := metadata[sdkmcp.MetaKeyServerInfo]; !exists {
				metadata[sdkmcp.MetaKeyServerInfo] = implementation
			}
			result.SetMeta(metadata)
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				return nil, sanitizedRPCError(marshalErr)
			}
			// The SDK adds the complete resultType after receiving middleware.
			const sdkResultTypeReserve = 64
			if len(encoded) > maxToolResponseBytes-sdkResultTypeReserve {
				return nil, sanitizedRPCError(errToolResultTooLarge)
			}
			return result, nil
		}
	}
}

// Run serves one MCP connection after wrapping it in the exact-version gate.
func (s *Server) Run(ctx context.Context, transport sdkmcp.Transport) error {
	if err := s.sdk.Run(ctx, exactTransport{Transport: transport}); err != nil {
		return fmt.Errorf("run MCP server: %w", err)
	}
	return nil
}

// HTTPHandler returns the protocol-level stateless HTTP handler. Command,
// authentication, origin, and listener policy are added by the transport task.
func (s *Server) HTTPHandler() http.Handler {
	return s.protocolHTTPHandler(true, nil)
}

func (s *Server) protocolHTTPHandler(jsonResponse bool, logger *slog.Logger) http.Handler {
	streamable := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return s.sdk
	}, &sdkmcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 jsonResponse,
		Logger:                       logger,
		MaxRequestBodyBytes:          maxHTTPRequestBytes,
		PropagateRequestCancellation: true,
	})
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response = &noStoreResponseWriter{ResponseWriter: response}
		if request.Method == http.MethodPost {
			if handled := validateHTTPRequest(response, request); handled {
				return
			}
		}
		streamable.ServeHTTP(response, request)
	})
}

func normalizeDiscovery(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil || method != "server/discover" {
			return result, err
		}
		discovery, ok := result.(*sdkmcp.DiscoverResult)
		if !ok {
			return nil, fmt.Errorf("server/discover returned %T", result)
		}
		discovery.SupportedVersions = []string{ProtocolVersion}
		discovery.TTLMs = discoveryTTLMs
		discovery.CacheScope = "public"
		return discovery, nil
	}
}

type exactTransport struct {
	sdkmcp.Transport
}

func (t exactTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect MCP transport: %w", err)
	}
	return &exactConnection{Connection: connection}, nil
}

func (t exactTransport) SupportsProtocolVersion(version string) bool {
	if version != ProtocolVersion {
		return false
	}
	if supporter, ok := t.Transport.(sdkmcp.ProtocolVersionSupporter); ok {
		return supporter.SupportsProtocolVersion(version)
	}
	return true
}

type exactConnection struct {
	sdkmcp.Connection
}

func (c *exactConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		message, err := c.Connection.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read MCP message: %w", err)
		}
		request, ok := message.(*jsonrpc.Request)
		if !ok {
			return message, nil
		}
		wireErr := validateRequest(request)
		if wireErr == nil {
			return message, nil
		}
		if request.IsCall() {
			if err := c.Write(ctx, &jsonrpc.Response{ID: request.ID, Error: wireErr}); err != nil {
				return nil, fmt.Errorf("write MCP rejection: %w", err)
			}
		}
	}
}

func validateRequest(request *jsonrpc.Request) *jsonrpc.Error {
	if request.Method == "initialize" {
		return unsupportedVersionError(initializeVersion(request.Params))
	}

	metadata, requested := requestMetadata(request.Params)
	if requested != ProtocolVersion {
		return unsupportedVersionError(requested)
	}
	capabilities, ok := metadata[sdkmcp.MetaKeyClientCapabilities]
	if !ok || !isJSONObject(capabilities) {
		return &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing or invalid _meta field %q", sdkmcp.MetaKeyClientCapabilities),
		}
	}
	return nil
}

func requestMetadata(params json.RawMessage) (map[string]json.RawMessage, string) {
	var envelope struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil || envelope.Meta == nil {
		return nil, ""
	}
	var version string
	if raw, ok := envelope.Meta[sdkmcp.MetaKeyProtocolVersion]; ok {
		_ = json.Unmarshal(raw, &version)
	}
	return envelope.Meta, version
}

func initializeVersion(params json.RawMessage) string {
	var initialize struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &initialize)
	return initialize.ProtocolVersion
}

func unsupportedVersionError(requested string) *jsonrpc.Error {
	data, err := json.Marshal(sdkmcp.UnsupportedProtocolVersionData{
		Supported: []string{ProtocolVersion},
		Requested: requested,
	})
	if err != nil {
		panic(err)
	}
	return &jsonrpc.Error{
		Code:    sdkmcp.CodeUnsupportedProtocolVersion,
		Message: fmt.Sprintf("Docbank supports only MCP protocol version %s; use server/discover", ProtocolVersion),
		Data:    data,
	}
}

func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func validateHTTPRequest(response http.ResponseWriter, request *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxHTTPRequestBytes+1))
	if err != nil {
		http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
		return true
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxHTTPRequestBytes {
		http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
		return true
	}

	message, err := jsonrpc.DecodeMessage(body)
	if err != nil {
		return false
	}
	wireRequest, ok := message.(*jsonrpc.Request)
	if !ok {
		return false
	}
	if wireErr := validateRequest(wireRequest); wireErr != nil {
		writeHTTPError(response, wireRequest.ID, wireErr)
		return true
	}
	_, bodyVersion := requestMetadata(wireRequest.Params)
	headerVersion := request.Header.Get("Mcp-Protocol-Version")
	if headerVersion == "" {
		writeHTTPError(response, wireRequest.ID, headerMismatch("missing required Mcp-Protocol-Version header"))
		return true
	}
	if headerVersion != bodyVersion {
		writeHTTPError(response, wireRequest.ID,
			headerMismatch(fmt.Sprintf("Mcp-Protocol-Version header value %q does not match body value %q", headerVersion, bodyVersion)))
		return true
	}
	if method := request.Header.Get("Mcp-Method"); method == "" {
		writeHTTPError(response, wireRequest.ID, headerMismatch("missing required Mcp-Method header"))
		return true
	} else if method != wireRequest.Method {
		writeHTTPError(response, wireRequest.ID,
			headerMismatch(fmt.Sprintf("Mcp-Method header value %q does not match body value %q", method, wireRequest.Method)))
		return true
	}
	if name, required := requestName(wireRequest); required {
		headerName := request.Header.Get("Mcp-Name")
		if headerName == "" {
			writeHTTPError(response, wireRequest.ID,
				headerMismatch(fmt.Sprintf("missing required Mcp-Name header for method %q", wireRequest.Method)))
			return true
		}
		if headerName != name {
			writeHTTPError(response, wireRequest.ID,
				headerMismatch(fmt.Sprintf("Mcp-Name header value %q does not match body value %q", headerName, name)))
			return true
		}
	}
	return false
}

func requestName(request *jsonrpc.Request) (string, bool) {
	var params struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return "", false
	}
	switch request.Method {
	case "tools/call", "prompts/get":
		return params.Name, true
	case "resources/read":
		return params.URI, true
	default:
		return "", false
	}
}

func headerMismatch(message string) *jsonrpc.Error {
	return &jsonrpc.Error{
		Code:    sdkmcp.CodeHeaderMismatch,
		Message: "HeaderMismatch: " + message,
	}
}

func writeHTTPError(response http.ResponseWriter, id jsonrpc.ID, wireErr *jsonrpc.Error) {
	body, err := jsonrpc.EncodeMessage(&jsonrpc.Response{ID: id, Error: wireErr})
	if err != nil {
		http.Error(response, "failed to encode JSON-RPC error", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusBadRequest)
	_, _ = response.Write(body)
}

type noStoreResponseWriter struct {
	http.ResponseWriter
}

func (w *noStoreResponseWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(status)
}

func (w *noStoreResponseWriter) Write(body []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	return w.ResponseWriter.Write(body)
}

func (w *noStoreResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
