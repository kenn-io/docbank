package api

import (
	"context"
	"crypto/subtle"
	"encoding/json/v2"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/daemonauth"
)

const requestTimeout = 60 * time.Second

type authenticationContextKey struct{}
type workspaceSnapshotOwnerContextKey struct{}

func workspaceSnapshotOwner(ctx context.Context) (string, bool) {
	owner, ok := ctx.Value(workspaceSnapshotOwnerContextKey{}).(string)
	return owner, ok && owner != ""
}

func browserSessionRequest(ctx context.Context) bool {
	authentication, _ := ctx.Value(authenticationContextKey{}).(string)
	return authentication == "browser"
}

// timeout-exempt: long-running maintenance, integrity reads, bulk ingest, and
// export preparation.
func timeoutExempt(method, path string) bool {
	if packageContainerTimeoutExempt(method, path) {
		return true
	}
	switch path {
	case "/api/v1/ingest", "/api/v1/ingest/stream", "/api/v1/ingest/preflight", "/api/v1/packages/preflights", "/api/v1/gc", "/api/v1/verify", "/api/v1/audit/verify", "/api/v1/trash/empty",
		"/api/v1/processing/jobs", "/api/v1/derivatives/purge-jobs",
		"/api/v1/exports/sources", "/api/v1/exports/plans", "/api/v1/bates/exports",
		"/api/v1/storage/pack", "/api/v1/storage/repack", "/api/v1/uploads",
		"/api/v1/backup/snapshots", "/api/v1/backup/snapshots/stream",
		"/api/v1/backup/verify", "/api/v1/backup/verify/stream",
		"/api/v1/backup/restore", "/api/v1/backup/restore/stream",
		webDownloadPreparePath, webDownloadFilePath, webUploadSocketPath:
		return true
	}
	if strings.HasPrefix(path, "/api/v1/mailbox/") {
		return mailboxTimeoutExempt(method, path)
	}
	if method == http.MethodPost {
		if id, ok := strings.CutPrefix(path, "/api/v1/exports/jobs/"); ok {
			if id, ok := strings.CutSuffix(id, "/download"); ok && (id == "{id}" || validPageJobPathID(id)) {
				return true
			}
		}
	}
	if strings.HasPrefix(path, "/api/v1/nodes/") &&
		(strings.HasSuffix(path, "/verify") || strings.HasSuffix(path, "/content")) {
		return true
	}
	if strings.HasPrefix(path, "/api/v1/exports/sources/") && strings.HasSuffix(path, "/seal") {
		return true
	}
	return strings.HasPrefix(path, "/api/v1/renditions/") ||
		(strings.HasPrefix(path, "/api/v1/versions/") && strings.HasSuffix(path, "/content")) ||
		isEmailPartPath(path)
}

func packageContainerTimeoutExempt(method, path string) bool {
	path, ok := strings.CutPrefix(path, "/api/v1/packages/containers/")
	if !ok {
		return false
	}
	parts := strings.Split(path, "/")
	return len(parts) == 2 && parts[0] != "" && method == http.MethodPost && (parts[1] == "seal" || parts[1] == "preflight") ||
		len(parts) == 3 && parts[0] != "" && method == http.MethodPut && parts[1] == "chunks" && parts[2] != ""
}

// Split before unescaping so IDs containing an encoded slash remain one segment,
// as they do in ServeMux routing. OpenAPI template paths use the same matcher.
func mailboxTimeoutExempt(method, path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		var err error
		parts[i], err = url.PathUnescape(part)
		if err != nil {
			return false
		}
	}
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "mailbox" {
		return false
	}
	parts = parts[3:]
	switch len(parts) {
	case 1:
		return method == http.MethodPost && parts[0] == "transfers"
	case 3:
		return parts[1] != "" &&
			((method == http.MethodPost && parts[0] == "containers" && parts[2] == "seal") ||
				(method == http.MethodGet && parts[0] == "jobs" && parts[2] == "events"))
	case 4:
		return method == http.MethodPut && parts[0] == "containers" && parts[1] != "" && parts[2] == "chunks" && parts[3] != ""
	}
	return false
}

func timeoutExemptRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/v1/mailbox/") {
		return mailboxTimeoutExempt(r.Method, r.URL.EscapedPath())
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/packages/containers/") {
		return packageContainerTimeoutExempt(r.Method, r.URL.EscapedPath())
	}
	if timeoutExempt(r.Method, r.URL.Path) {
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	if r.URL.Path == "/api/v1/media/sources" {
		return true
	}
	if rest, found := strings.CutPrefix(r.URL.Path, "/api/v1/media/sources/"); found {
		sourceID, action, _ := strings.Cut(rest, "/")
		return sourceID != "" && (action == "artifacts" || action == "retry")
	}
	rest, found := strings.CutPrefix(r.URL.Path, "/api/v1/versions/")
	if !found {
		return false
	}
	segments := strings.Split(rest, "/")
	return len(segments) == 2 && segments[0] != "" && segments[1] == "email"
}

func isEmailPartPath(path string) bool {
	rest, found := strings.CutPrefix(path, "/api/v1/versions/")
	if !found {
		return false
	}
	segments := strings.Split(rest, "/")
	return len(segments) == 7 && segments[0] != "" && segments[1] == "email" &&
		segments[2] == "generations" && segments[3] != "" && segments[4] == "parts" &&
		segments[5] != "" && segments[6] != ""
}

// clearLongRunningBodyReadDeadlines keeps Huma's request-body deadline in
// lockstep with Docbank's handler timeout policy. Retaining that five-second
// socket deadline after decoding can only cancel valid long-running handler
// work.
func clearLongRunningBodyReadDeadlines(api huma.API) {
	for path, item := range api.OpenAPI().Paths {
		for _, operation := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if operation != nil && timeoutExempt(operation.Method, path) {
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
	case "/", "/health", kitPingPath, daemonauth.ChallengePath,
		webDownloadFilePath, webUploadSocketPath:
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
	_ = json.MarshalWrite(w, e)
}

// authMiddleware requires key on every non-exempt route. There is no
// keyless bypass: NewServer refuses to build a server with an empty key
// (the offline OpenAPI-document path is the only caller that doesn't serve
// requests, and it supplies a placeholder key), so key is always set here.
func authMiddleware(next http.Handler, key string, sessions *webSessionRegistry, masterOwner string) http.Handler {
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
			ctx := context.WithValue(r.Context(), authenticationContextKey{}, "master")
			ctx = context.WithValue(ctx, workspaceSnapshotOwnerContextKey{}, masterOwner)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		webToken := r.Header.Get(WebSessionHeader)
		if owner, sessionCtx, ok := sessions.authenticate(webToken); sessions != nil && ok {
			if !webSessionRequestAllowed(r) {
				writeError(w, NewError(http.StatusForbidden, "web_session_read_only",
					"browser sessions cannot use this endpoint"))
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(sessionCtx, cancel)
			defer stop()
			defer cancel()
			ctx = context.WithValue(ctx, authenticationContextKey{}, "browser")
			ctx = context.WithValue(ctx, workspaceSnapshotOwnerContextKey{}, owner)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		writeError(w, NewError(http.StatusUnauthorized, "unauthorized",
			"missing or invalid API key or browser session"))
	})
}

// loopbackMiddleware fences endpoints that grant local-filesystem
// capability (ingest and package preflight) to loopback peers, regardless of
// bind address or key.
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
	return path == "/api/v1/ingest" || path == "/api/v1/ingest/stream" || path == "/api/v1/ingest/preflight" || path == "/api/v1/packages/preflights"
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
		if timeoutExemptRequest(r) {
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
