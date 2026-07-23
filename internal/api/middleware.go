package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/jsontext"
)

const requestTimeout = 60 * time.Second

// timeout-exempt: long-running maintenance, integrity reads, and bulk ingest.
func timeoutExempt(path string) bool {
	switch path {
	case "/api/v1/ingest", "/api/v1/ingest/stream", "/api/v1/ingest/preflight", "/api/v1/gc", "/api/v1/verify", "/api/v1/audit/verify", "/api/v1/trash/empty",
		"/api/v1/storage/pack", "/api/v1/storage/repack", "/api/v1/uploads",
		"/api/v1/backup/snapshots", "/api/v1/backup/snapshots/stream",
		"/api/v1/backup/verify", "/api/v1/backup/verify/stream",
		"/api/v1/backup/restore", "/api/v1/backup/restore/stream":
		return true
	}
	if strings.HasPrefix(path, "/api/v1/nodes/") &&
		(strings.HasSuffix(path, "/verify") || strings.HasSuffix(path, "/content")) {
		return true
	}
	return strings.HasPrefix(path, "/api/v1/versions/") && strings.HasSuffix(path, "/content")
}

// clearLongRunningBodyReadDeadlines keeps Huma's request-body deadline in
// lockstep with Docbank's handler timeout policy. JSON mutation bodies have
// already been read and text-validated by jsonBodyTextMiddleware before Huma
// installs its deadline and dispatches them, so retaining that five-second
// socket deadline can only cancel valid long-running handler work.
func clearLongRunningBodyReadDeadlines(api huma.API) {
	for path, item := range api.OpenAPI().Paths {
		if !timeoutExempt(path) {
			continue
		}
		for _, operation := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if operation != nil {
				operation.BodyReadTimeout = -1
			}
		}
	}
}

// auth-exempt: discovery, credential-free ownership proof, docs, and static
// web assets carry no vault data. Everything else requires the key —
// the daemon always has one; see NewServer.
func authExempt(path string) bool {
	switch path {
	case "/", "/health", kitPingPath, daemonauth.ChallengePath:
		return true
	}
	return strings.HasPrefix(path, "/docs") ||
		strings.HasPrefix(path, "/openapi") ||
		strings.HasPrefix(path, "/schemas") ||
		strings.HasPrefix(path, "/assets/")
}

func writeError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	// Status and headers are already committed; there is nothing left to do
	// if encoding this fixed-shape struct somehow fails.
	_ = json.NewEncoder(w).Encode(e) //nolint:errchkjson // see above
}

// authMiddleware requires key on every non-exempt route. There is no
// keyless bypass: NewServer refuses to build a server with an empty key
// (the offline OpenAPI-document path is the only caller that doesn't serve
// requests, and it supplies a placeholder key), so key is always set here.
func authMiddleware(next http.Handler, key string, sessions *webSessionRegistry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		webToken := r.Header.Get(WebSessionHeader)
		if sessions != nil && sessions.valid(webToken) {
			if !webSessionRequestAllowed(r.Method, r.URL.Path) {
				writeError(w, NewError(http.StatusForbidden, "web_session_read_only",
					"browser sessions cannot use this endpoint"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, NewError(http.StatusUnauthorized, "unauthorized",
			"missing or invalid API key or browser session"))
	})
}

// loopbackMiddleware fences endpoints that grant local-filesystem
// capability (POST /api/v1/ingest and its preflight) to loopback peers, regardless of bind
// address or key. See the spec's ingest addendum.
func loopbackMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && isServerPathIngestRoute(r.URL.Path) && !isLoopbackRemote(r.RemoteAddr) {
			writeError(w, NewError(http.StatusForbidden, "loopback_only",
				"server-side path ingest is loopback-only; remote clients use POST /api/v1/uploads"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isServerPathIngestRoute(path string) bool {
	return path == "/api/v1/ingest" || path == "/api/v1/ingest/stream" || path == "/api/v1/ingest/preflight"
}

// jsonBodyTextMiddleware runs before Huma's JSON decoder, which follows
// encoding/json's lossy behavior and replaces invalid UTF-8 or unpaired
// surrogate escapes with U+FFFD. Names and paths must reach route validation
// byte-for-byte or a mutation could target a different, replacement-character
// node. Every mutating request is text-validated regardless of Content-Type:
// Huma accepts an omitted Content-Type for body-bound operations, and malformed
// headers must not bypass the boundary. Explicit content-write routes carry
// opaque bytes and validate their own size, digest, and transport envelope.
func jsonBodyTextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || !mutationMethod(r.Method) || isOpaqueBodyMutation(r) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			writeError(w, NewError(http.StatusBadRequest, "validation", "could not read request body"))
			return
		}
		if err := jsontext.Validate(body, "request body"); err != nil {
			writeError(w, NewError(http.StatusBadRequest, "validation", err.Error()))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func isOpaqueBodyMutation(r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/uploads" {
		return true
	}
	return r.Method == http.MethodPut &&
		strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") &&
		strings.HasSuffix(r.URL.Path, "/content")
}

func mutationMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func timeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if timeoutExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func trackMiddleware(next http.Handler, t *ActivityTracker) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Begin()
		defer t.End()
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr, "duration", time.Since(start))
	})
}

func recoverMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeError(w, NewError(http.StatusInternalServerError, "internal", "internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
