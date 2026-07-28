package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	webSessionPath   = "/api/daemon/web-session"
	WebSessionHeader = "X-Docbank-Web-Session"
)

// webSessionRegistry owns browser credentials for exactly one daemon
// lifetime. Tokens are random, retained only as digests, and authorize only
// the deliberately limited routes used by the built-in browser. Most are
// reads; verified upload is the one document-authority mutation.
type webSessionRegistry struct {
	mu     sync.Mutex
	tokens map[[sha256.Size]byte]struct{}
}

func newWebSessionRegistry() *webSessionRegistry {
	return &webSessionRegistry{tokens: make(map[[sha256.Size]byte]struct{})}
}

func (r *webSessionRegistry) issue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating browser session: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	r.mu.Lock()
	r.tokens[sha256.Sum256([]byte(token))] = struct{}{}
	r.mu.Unlock()
	return token, nil
}

func (r *webSessionRegistry) valid(token string) bool {
	if token == "" {
		return false
	}
	r.mu.Lock()
	_, ok := r.tokens[sha256.Sum256([]byte(token))]
	r.mu.Unlock()
	return ok
}

func (r *webSessionRegistry) revoke(token string) {
	r.mu.Lock()
	delete(r.tokens, sha256.Sum256([]byte(token)))
	r.mu.Unlock()
}

func webSessionRequestAllowed(r *http.Request) bool {
	method, path := r.Method, r.URL.Path
	if method == http.MethodPost && path == webDownloadPreparePath {
		return true
	}
	if method == http.MethodPost && path == "/api/v1/uploads" {
		return true
	}
	if method == http.MethodDelete && path == webSessionPath {
		return true
	}
	if method != http.MethodGet {
		return false
	}
	switch path {
	case "/api/v1/path", "/api/v1/search",
		"/api/v1/audit/status", "/api/v1/audit/history", "/api/v1/jobs",
		"/api/v1/storage", "/api/v1/tags":
		return true
	case "/api/v1/backup/snapshots":
		// Browser sessions may inspect only the repository selected by daemon
		// configuration. An arbitrary repo query is a server-filesystem read
		// capability and remains exclusive to the master API credential.
		return r.URL.RawQuery == ""
	}
	const tagPrefix = "/api/v1/tags/"
	if after, ok := strings.CutPrefix(path, tagPrefix); ok {
		parts := strings.Split(after, "/")
		return parts[0] != "" &&
			(len(parts) == 1 || (len(parts) == 2 && parts[1] == "nodes"))
	}
	const prefix = "/api/v1/nodes/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 1 || len(parts) > 2 {
		return false
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return false
	}
	return len(parts) == 1 || parts[1] == "children" ||
		parts[1] == "versions" || parts[1] == "provenance" ||
		parts[1] == "tags"
}

func registerWebSession(
	mux *http.ServeMux,
	enabled bool,
	webURL string,
	sessions *webSessionRegistry,
) {
	mux.HandleFunc("POST "+webSessionPath, func(w http.ResponseWriter, _ *http.Request) {
		if !enabled || webURL == "" {
			writeError(w, NewError(http.StatusServiceUnavailable, "web_unavailable",
				"this daemon is not serving the compiled web application"))
			return
		}
		token, err := sessions.issue()
		if err != nil {
			writeError(w, NewError(http.StatusInternalServerError, "internal",
				"could not create a browser session"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, struct {
			Token string `json:"token"`
			URL   string `json:"url"`
		}{Token: token, URL: webURL})
	})
	mux.HandleFunc("DELETE "+webSessionPath, func(w http.ResponseWriter, r *http.Request) {
		sessions.revoke(r.Header.Get(WebSessionHeader))
		w.WriteHeader(http.StatusNoContent)
	})
}
