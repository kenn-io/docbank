package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

const (
	webSessionPath   = "/api/daemon/web-session"
	WebSessionHeader = "X-Docbank-Web-Session"
)

// webSessionRegistry owns browser credentials for exactly one daemon
// lifetime. Tokens are random, retained only as digests, and authorize only
// the deliberately limited routes used by the built-in browser. Most are
// reads; verified upload plus revision-bound trash and restore are the only
// document-authority mutations.
type webSessionRegistry struct {
	mu          sync.Mutex
	tokens      map[[sha256.Size]byte]webSessionState
	uploads     map[*websocket.Conn]struct{}
	uploadGroup sync.WaitGroup
	closing     bool
}

type webSessionState struct {
	uploadSecret [sha256.Size]byte
	upload       *websocket.Conn
}

func newWebSessionRegistry() *webSessionRegistry {
	return &webSessionRegistry{
		tokens:  make(map[[sha256.Size]byte]webSessionState),
		uploads: make(map[*websocket.Conn]struct{}),
	}
}

func (r *webSessionRegistry) issue() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generating browser session: %w", err)
	}
	var uploadSecret [sha256.Size]byte
	if _, err := rand.Read(uploadSecret[:]); err != nil {
		return "", "", fmt.Errorf("generating browser upload secret: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return "", "", errors.New("browser sessions are shutting down")
	}
	r.tokens[sha256.Sum256([]byte(token))] = webSessionState{uploadSecret: uploadSecret}
	r.mu.Unlock()
	return token, base64.RawURLEncoding.EncodeToString(uploadSecret[:]), nil
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

func (r *webSessionRegistry) uploadSecret(token string) ([sha256.Size]byte, bool) {
	if token == "" {
		return [sha256.Size]byte{}, false
	}
	r.mu.Lock()
	state, ok := r.tokens[sha256.Sum256([]byte(token))]
	r.mu.Unlock()
	return state.uploadSecret, ok
}

func (r *webSessionRegistry) bindUpload(token string, conn *websocket.Conn) bool {
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.tokens[digest]
	if r.closing || !ok || state.upload != nil {
		return false
	}
	state.upload = conn
	r.tokens[digest] = state
	return true
}

func (r *webSessionRegistry) trackUpload(conn *websocket.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.uploadGroup.Add(1)
	r.uploads[conn] = struct{}{}
	return true
}

func (r *webSessionRegistry) releaseTrackedUpload(conn *websocket.Conn) {
	r.mu.Lock()
	if _, ok := r.uploads[conn]; !ok {
		r.mu.Unlock()
		return
	}
	delete(r.uploads, conn)
	r.mu.Unlock()
	r.uploadGroup.Done()
}

func (r *webSessionRegistry) releaseUpload(token string, conn *websocket.Conn) {
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.tokens[digest]
	if ok && state.upload == conn {
		state.upload = nil
		r.tokens[digest] = state
	}
}

func (r *webSessionRegistry) revoke(token string) {
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	state := r.tokens[digest]
	delete(r.tokens, digest)
	r.mu.Unlock()
	if state.upload != nil {
		_ = state.upload.CloseNow()
	}
}

func (r *webSessionRegistry) closeAll(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	clear(r.tokens)
	conns := make([]*websocket.Conn, 0, len(r.uploads))
	for conn := range r.uploads {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.CloseNow()
	}

	drained := make(chan struct{})
	go func() {
		r.uploadGroup.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for browser upload handlers: %w", ctx.Err())
	}
}

func webSessionRequestAllowed(r *http.Request) bool {
	method, path := r.Method, r.URL.Path
	if method == http.MethodPost && path == webDownloadPreparePath {
		return true
	}
	if method == http.MethodPost && r.URL.RawQuery == "" {
		const prefix = "/api/v1/nodes/"
		if after, ok := strings.CutPrefix(path, prefix); ok {
			parts := strings.Split(after, "/")
			nodeID, err := strconv.ParseInt(parts[0], 10, 64)
			if len(parts) == 2 &&
				(parts[1] == "trash" || parts[1] == "restore") &&
				err == nil && nodeID > 0 {
				return true
			}
		}
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
		"/api/v1/storage", "/api/v1/tags", "/api/v1/trash":
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
		token, uploadSecret, err := sessions.issue()
		if err != nil {
			writeError(w, NewError(http.StatusInternalServerError, "internal",
				"could not create a browser session"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, struct {
			Token        string `json:"token"`
			UploadSecret string `json:"upload_secret"`
			URL          string `json:"url"`
		}{Token: token, UploadSecret: uploadSecret, URL: webURL})
	})
	mux.HandleFunc("DELETE "+webSessionPath, func(w http.ResponseWriter, r *http.Request) {
		sessions.revoke(r.Header.Get(WebSessionHeader))
		w.WriteHeader(http.StatusNoContent)
	})
}
