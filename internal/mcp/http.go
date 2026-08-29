package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/client"
)

const (
	maxHTTPRequestBytes      = 1 << 20
	maxHTTPHeaderBytes       = 32 << 10
	maxHTTPHeaderFields      = 128
	maxHTTPBearerBytes       = 4096
	maxHTTPResponseBytes     = maxToolResponseBytes + 64<<10
	httpResponseTooLargeBody = "MCP response too large\n"

	defaultHTTPConcurrentRequests  = 32
	defaultHTTPConcurrentPerClient = 4
	defaultHTTPRequestTimeout      = 2 * time.Minute
	defaultHTTPShutdownTimeout     = 10 * time.Second
)

var (
	errHTTPConfiguration    = errors.New("invalid MCP HTTP configuration")
	errHTTPResponseTooLarge = errors.New("MCP HTTP response exceeds the configured limit")
)

// HTTPLimits bounds every resource controlled by an inbound MCP HTTP request.
// Zero fields receive the package defaults.
type HTTPLimits struct {
	MaxRequestBytes        int
	MaxHeaderBytes         int
	MaxHeaderFields        int
	MaxConcurrentRequests  int
	MaxConcurrentPerClient int
	MaxResponseBytes       int
	RequestTimeout         time.Duration
}

// HTTPOptions configures the authenticated stateless HTTP boundary. The bearer
// value must already have been resolved from the named config binding.
type HTTPOptions struct {
	BearerToken  string
	JSONResponse bool
	Logger       *slog.Logger
	Limits       HTTPLimits
}

// ValidateHTTPListenAddress rejects hostnames, wildcards, missing ports, and
// non-loopback addresses before a listener is opened.
func ValidateHTTPListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" || portText == "" {
		return errors.New("MCP HTTP listen address must be an explicit loopback IP and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.IsLoopback() {
		return errors.New("MCP HTTP listen address must use a loopback IP")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return errors.New("MCP HTTP listen address has an invalid port")
	}
	_ = port // Port zero remains useful for explicitly requested test listeners.
	return nil
}

// HTTPTransportHandler wraps the official stateless SDK handler in Docbank's
// local origin, authentication, resource, cancellation, and telemetry gates.
func (server *Server) HTTPTransportHandler(options HTTPOptions) (http.Handler, error) {
	if server == nil {
		return nil, errHTTPConfiguration
	}
	// SDK diagnostics can contain transport internals. Docbank owns the stable,
	// redacted telemetry outside this handler instead.
	inner := server.protocolHTTPHandler(options.JSONResponse, nil)
	return wrapHTTPTransport(inner, options)
}

// prepareHTTP binds one opaque credential-exclusion policy to the daemon lease
// before acquiring the initial ownership-proven client. Every later lease
// acquisition and replacement remains subject to the same policy.
func (server *Server) prepareHTTP(ctx context.Context, bearer string) error {
	if server == nil || !validBearerToken(bearer) {
		return errHTTPConfiguration
	}
	policy := client.NewAPIKeyExclusionPolicy(bearer)
	if err := server.daemon.bindAPIKeyExclusion(policy); err != nil {
		return err
	}
	_, err := server.daemon.acquire(ctx)
	return err
}

func wrapHTTPTransport(inner http.Handler, options HTTPOptions) (http.Handler, error) {
	if inner == nil || !validBearerToken(options.BearerToken) {
		return nil, errHTTPConfiguration
	}
	limits, err := normalizeHTTPLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	guard := &httpTransportGuard{
		inner: inner, tokenHash: sha256.Sum256([]byte(options.BearerToken)),
		logger: logger, limits: limits, activeByClient: make(map[string]int),
	}
	return guard, nil
}

func validBearerToken(token string) bool {
	if token == "" || len(token) > maxHTTPBearerBytes {
		return false
	}
	for _, value := range []byte(token) {
		if value <= ' ' || value == 0x7f {
			return false
		}
	}
	return true
}

func normalizeHTTPLimits(limits HTTPLimits) (HTTPLimits, error) {
	defaults := HTTPLimits{
		MaxRequestBytes: maxHTTPRequestBytes, MaxHeaderBytes: maxHTTPHeaderBytes,
		MaxHeaderFields: maxHTTPHeaderFields, MaxConcurrentRequests: defaultHTTPConcurrentRequests,
		MaxConcurrentPerClient: defaultHTTPConcurrentPerClient,
		MaxResponseBytes:       maxHTTPResponseBytes, RequestTimeout: defaultHTTPRequestTimeout,
	}
	if limits.MaxRequestBytes == 0 {
		limits.MaxRequestBytes = defaults.MaxRequestBytes
	}
	if limits.MaxHeaderBytes == 0 {
		limits.MaxHeaderBytes = defaults.MaxHeaderBytes
	}
	if limits.MaxHeaderFields == 0 {
		limits.MaxHeaderFields = defaults.MaxHeaderFields
	}
	if limits.MaxConcurrentRequests == 0 {
		limits.MaxConcurrentRequests = defaults.MaxConcurrentRequests
	}
	if limits.MaxConcurrentPerClient == 0 {
		limits.MaxConcurrentPerClient = defaults.MaxConcurrentPerClient
	}
	if limits.MaxResponseBytes == 0 {
		limits.MaxResponseBytes = defaults.MaxResponseBytes
	}
	if limits.RequestTimeout == 0 {
		limits.RequestTimeout = defaults.RequestTimeout
	}
	if limits.MaxRequestBytes < 1 || limits.MaxRequestBytes > maxHTTPRequestBytes ||
		limits.MaxHeaderBytes < 1 || limits.MaxHeaderBytes > maxHTTPHeaderBytes ||
		limits.MaxHeaderFields < 1 || limits.MaxHeaderFields > maxHTTPHeaderFields ||
		limits.MaxConcurrentRequests < 1 ||
		limits.MaxConcurrentPerClient < 1 ||
		limits.MaxConcurrentPerClient > limits.MaxConcurrentRequests ||
		limits.MaxResponseBytes < len(httpResponseTooLargeBody) ||
		limits.MaxResponseBytes > maxHTTPResponseBytes ||
		limits.RequestTimeout < time.Millisecond || limits.RequestTimeout > 5*time.Minute {
		return HTTPLimits{}, errHTTPConfiguration
	}
	return limits, nil
}

type httpTransportGuard struct {
	inner     http.Handler
	tokenHash [sha256.Size]byte
	logger    *slog.Logger
	limits    HTTPLimits

	activityMu     sync.Mutex
	activeTotal    int
	activeByClient map[string]int
}

func (guard *httpTransportGuard) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	started := time.Now()
	observed := &observedResponseWriter{ResponseWriter: response}
	observed.Header().Set("Cache-Control", "no-store")
	errorCode := "ok"
	defer func() {
		guard.logger.Info("MCP HTTP request",
			"request_count", 1,
			"latency_ms", time.Since(started).Milliseconds(),
			"response_bytes", observed.bytes,
			"error_code", errorCode,
		)
	}()

	if !headersWithin(request.Header, guard.limits.MaxHeaderFields, guard.limits.MaxHeaderBytes) {
		errorCode = "headers_too_large"
		http.Error(observed, "request headers too large", http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	if !safeLocalOrigin(request) {
		errorCode = "origin_forbidden"
		http.Error(observed, "forbidden origin", http.StatusForbidden)
		return
	}
	if request.Method != http.MethodPost {
		errorCode = "method_not_allowed"
		observed.Header().Set("Allow", http.MethodPost)
		http.Error(observed, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if headerPresent(request.Header, "Mcp-Session-Id") || headerPresent(request.Header, "Last-Event-ID") {
		errorCode = "state_not_supported"
		http.Error(observed, "HTTP sessions and resumption are not supported", http.StatusBadRequest)
		return
	}
	if !guard.authenticated(request.Header) {
		errorCode = "unauthorized"
		observed.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(observed, "unauthorized", http.StatusUnauthorized)
		return
	}
	if request.ContentLength > int64(guard.limits.MaxRequestBytes) {
		errorCode = "request_too_large"
		http.Error(observed, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	clientID := httpClientID(request.RemoteAddr)
	if !guard.acquire(clientID) {
		errorCode = "activity_limited"
		http.Error(observed, "too many active requests", http.StatusTooManyRequests)
		return
	}
	defer guard.release(clientID)

	deadline := time.Now().Add(guard.limits.RequestTimeout)
	controller := http.NewResponseController(response)
	readDeadlineSet, err := setHTTPDeadline(controller.SetReadDeadline, deadline)
	if err != nil {
		errorCode = "transport_failure"
		http.Error(observed, "request transport unavailable", http.StatusInternalServerError)
		return
	}
	writeDeadlineSet, err := setHTTPDeadline(controller.SetWriteDeadline, deadline)
	if err != nil {
		if readDeadlineSet {
			_ = controller.SetReadDeadline(time.Time{})
		}
		errorCode = "transport_failure"
		http.Error(observed, "request transport unavailable", http.StatusInternalServerError)
		return
	}
	defer func() {
		if readDeadlineSet {
			_ = controller.SetReadDeadline(time.Time{})
		}
		if writeDeadlineSet {
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}()

	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	defer cancel()
	request = request.WithContext(ctx)
	request.Body = http.MaxBytesReader(observed, request.Body, int64(guard.limits.MaxRequestBytes))
	bounded := &boundedResponseWriter{
		ResponseWriter: observed, limit: guard.limits.MaxResponseBytes, cancel: cancel,
	}
	guard.inner.ServeHTTP(bounded, request)
	writeErr := bounded.finish()
	if bounded.exceeded {
		errorCode = "response_too_large"
	} else if ctx.Err() != nil || writeErr != nil {
		errorCode = "request_cancelled"
	} else if observed.status >= http.StatusBadRequest {
		errorCode = stableHTTPStatusCode(observed.status)
	}
}

func setHTTPDeadline(set func(time.Time) error, deadline time.Time) (bool, error) {
	err := set(deadline)
	if errors.Is(err, http.ErrNotSupported) {
		return false, nil
	}
	return err == nil, err
}

func headerPresent(header http.Header, key string) bool {
	for name := range header {
		if strings.EqualFold(name, key) {
			return true
		}
	}
	return false
}

func headersWithin(header http.Header, maxFields, maxBytes int) bool {
	fields := 0
	total := 0
	for name, values := range header {
		fields += len(values)
		total += len(name)
		for _, value := range values {
			total += len(value)
		}
		if fields > maxFields || total > maxBytes {
			return false
		}
	}
	return true
}

type localHTTPAuthority struct {
	host string
	port uint16
}

func safeLocalOrigin(request *http.Request) bool {
	requestAuthority, ok := parseLocalHTTPAuthority(request.Host)
	if !ok {
		return false
	}
	values := request.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	origin, err := url.Parse(values[0])
	if err != nil || !strings.EqualFold(origin.Scheme, "http") || origin.User != nil ||
		origin.Host == "" || origin.Path != "" || origin.RawPath != "" || origin.RawQuery != "" ||
		origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" {
		return false
	}
	originAuthority, ok := parseLocalHTTPAuthority(origin.Host)
	return ok && originAuthority == requestAuthority
}

func parseLocalHTTPAuthority(value string) (localHTTPAuthority, bool) {
	if value == "" || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "/@?#\\") {
		return localHTTPAuthority{}, false
	}
	host := value
	portText := ""
	hasPort := false
	if strings.HasPrefix(value, "[") {
		closing := strings.IndexByte(value, ']')
		if closing < 0 {
			return localHTTPAuthority{}, false
		}
		host = value[1:closing]
		remainder := value[closing+1:]
		if remainder != "" {
			if !strings.HasPrefix(remainder, ":") || len(remainder) == 1 {
				return localHTTPAuthority{}, false
			}
			hasPort = true
			portText = remainder[1:]
		}
	} else if strings.Count(value, ":") > 1 {
		return localHTTPAuthority{}, false
	} else if before, after, found := strings.Cut(value, ":"); found {
		hasPort = true
		host, portText = before, after
	}
	if host == "" {
		return localHTTPAuthority{}, false
	}
	port := uint64(80)
	if hasPort {
		if portText == "" {
			return localHTTPAuthority{}, false
		}
		parsed, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || parsed == 0 {
			return localHTTPAuthority{}, false
		}
		port = parsed
	}
	if strings.EqualFold(host, "localhost") {
		return localHTTPAuthority{host: "localhost", port: uint16(port)}, true
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return localHTTPAuthority{}, false
	}
	if !address.Unmap().IsLoopback() {
		return localHTTPAuthority{}, false
	}
	return localHTTPAuthority{host: address.String(), port: uint16(port)}, true
}

func (guard *httpTransportGuard) authenticated(header http.Header) bool {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	candidate, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || !validBearerToken(candidate) {
		return false
	}
	candidateHash := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(candidateHash[:], guard.tokenHash[:]) == 1
}

func (guard *httpTransportGuard) acquire(clientID string) bool {
	guard.activityMu.Lock()
	defer guard.activityMu.Unlock()
	if guard.activeTotal >= guard.limits.MaxConcurrentRequests ||
		guard.activeByClient[clientID] >= guard.limits.MaxConcurrentPerClient {
		return false
	}
	guard.activeTotal++
	guard.activeByClient[clientID]++
	return true
}

func (guard *httpTransportGuard) release(clientID string) {
	guard.activityMu.Lock()
	defer guard.activityMu.Unlock()
	guard.activeTotal--
	guard.activeByClient[clientID]--
	if guard.activeByClient[clientID] == 0 {
		delete(guard.activeByClient, clientID)
	}
}

func httpClientID(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return "unknown"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return ip.Unmap().String()
}

func stableHTTPStatusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "activity_limited"
	default:
		return "http_error"
	}
}

type observedResponseWriter struct {
	http.ResponseWriter

	status int
	bytes  int
}

func (writer *observedResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *observedResponseWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	written, err := writer.ResponseWriter.Write(body)
	writer.bytes += written
	return written, err
}

func (writer *observedResponseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (writer *observedResponseWriter) Flush() {
	_ = http.NewResponseController(writer.ResponseWriter).Flush()
}

type boundedResponseWriter struct {
	http.ResponseWriter

	limit     int
	cancel    context.CancelFunc
	status    int
	buffer    bytes.Buffer
	pending   []byte
	emitted   int
	streaming bool
	committed bool
	exceeded  bool
}

func (writer *boundedResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.streaming = isSSEContentType(writer.Header().Get("Content-Type"))
}

func (writer *boundedResponseWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.streaming || isSSEContentType(writer.Header().Get("Content-Type")) {
		writer.streaming = true
		return writer.writeSSE(body)
	}
	if writer.buffer.Len()+len(body) > writer.limit {
		writer.exceeded = true
		writer.cancel()
		return 0, errHTTPResponseTooLarge
	}
	_, _ = writer.buffer.Write(body)
	return len(body), nil
}

func (writer *boundedResponseWriter) writeSSE(body []byte) (int, error) {
	originalLength := len(body)
	for len(body) > 0 {
		remaining := writer.limit - writer.emitted - len(writer.pending)
		accepted := min(len(body), remaining+1)
		writer.pending = append(writer.pending, body[:accepted]...)
		body = body[accepted:]
		for {
			eventEnd := completeSSEEventEnd(writer.pending)
			if eventEnd == 0 {
				break
			}
			if writer.emitted+eventEnd > writer.limit {
				writer.pending = writer.pending[:0]
				writer.exceeded = true
				writer.cancel()
				return originalLength, errHTTPResponseTooLarge
			}
			writer.commit()
			written, err := writer.ResponseWriter.Write(writer.pending[:eventEnd])
			writer.emitted += written
			writer.pending = writer.pending[eventEnd:]
			if err != nil || written != eventEnd {
				writer.cancel()
				if err == nil {
					err = io.ErrShortWrite
				}
				return originalLength, err
			}
		}
		if writer.emitted+len(writer.pending) > writer.limit {
			writer.pending = writer.pending[:0]
			writer.exceeded = true
			writer.cancel()
			return originalLength, errHTTPResponseTooLarge
		}
	}
	return originalLength, nil
}

func completeSSEEventEnd(body []byte) int {
	lf := bytes.Index(body, []byte("\n\n"))
	crlf := bytes.Index(body, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return 0
	case lf < 0:
		return crlf + 4
	case crlf < 0:
		return lf + 2
	case lf < crlf:
		return lf + 2
	default:
		return crlf + 4
	}
}

func isSSEContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func (writer *boundedResponseWriter) commit() {
	if writer.committed {
		return
	}
	writer.committed = true
	writer.Header().Set("Cache-Control", "no-store")
	status := writer.status
	if status == 0 {
		status = http.StatusOK
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *boundedResponseWriter) finish() error {
	if writer.streaming {
		if len(writer.pending) != 0 {
			writer.pending = writer.pending[:0]
			writer.exceeded = true
			writer.cancel()
			return errHTTPResponseTooLarge
		}
		writer.commit()
		return nil
	}
	if writer.exceeded {
		clearHTTPResponseHeaders(writer.Header())
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.status = http.StatusInternalServerError
		writer.commit()
		_, err := writer.ResponseWriter.Write([]byte(httpResponseTooLargeBody))
		return err
	}
	writer.commit()
	_, err := writer.ResponseWriter.Write(writer.buffer.Bytes())
	return err
}

func clearHTTPResponseHeaders(header http.Header) {
	for name := range header {
		header.Del(name)
	}
}

func (writer *boundedResponseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (writer *boundedResponseWriter) Flush() {
	if isSSEContentType(writer.Header().Get("Content-Type")) {
		writer.streaming = true
		writer.commit()
		_ = http.NewResponseController(writer.ResponseWriter).Flush()
	}
}

// ServeHTTP listens on one explicit loopback address until ctx is cancelled.
func ServeHTTP(ctx context.Context, server *Server, address string, options HTTPOptions) error {
	if err := ValidateHTTPListenAddress(address); err != nil {
		return err
	}
	if err := server.prepareHTTP(ctx, options.BearerToken); err != nil {
		return err
	}
	handler, err := server.HTTPTransportHandler(options)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for MCP HTTP: %w", err)
	}
	defer func() { _ = listener.Close() }()
	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: maxHTTPHeaderBytes,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	select {
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve MCP HTTP: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultHTTPShutdownTimeout)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		serveErr := <-serveDone
		if shutdownErr != nil {
			return fmt.Errorf("shut down MCP HTTP: %w", shutdownErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve MCP HTTP: %w", serveErr)
		}
		return nil
	}
}
