package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const requestTimeout = 60 * time.Second

// timeout-exempt: long-running maintenance and bulk ingest.
func timeoutExempt(path string) bool {
	switch path {
	case "/api/v1/ingest", "/api/v1/gc", "/api/v1/verify", "/api/v1/trash/empty",
		"/api/v1/storage/pack":
		return true
	}
	return false
}

// auth-exempt: discovery, docs, and the static placeholder carry no vault
// data. Everything else requires the key — the daemon always has one; see
// NewServer.
func authExempt(path string) bool {
	switch path {
	case "/", "/health", kitPingPath:
		return true
	}
	return strings.HasPrefix(path, "/docs") ||
		strings.HasPrefix(path, "/openapi") ||
		strings.HasPrefix(path, "/schemas")
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
func authMiddleware(next http.Handler, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			writeError(w, NewError(http.StatusUnauthorized, "unauthorized", "missing or invalid API key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackMiddleware fences endpoints that grant local-filesystem
// capability (POST /api/v1/ingest) to loopback peers, regardless of bind
// address or key. See the spec's ingest addendum.
func loopbackMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/ingest" && !isLoopbackRemote(r.RemoteAddr) {
			writeError(w, NewError(http.StatusForbidden, "loopback_only",
				"ingest by server-side path is loopback-only; remote clients need multipart upload (planned)"))
			return
		}
		next.ServeHTTP(w, r)
	})
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
