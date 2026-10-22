package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/telemetry"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	webSessionPath   = "/api/daemon/web-session"
	WebSessionHeader = "X-Docbank-Web-Session"
	webLimitQuery    = "limit"
)

// webSessionRegistry owns browser credentials for exactly one daemon
// lifetime. Tokens are random, retained only as digests, and authorize only
// the deliberately limited routes used by the built-in browser. Most are
// reads; mutations cover verified upload, revision-bound trash, restore, and
// tag assignment, tag and saved-definition management, processing execution,
// and operator-scope processing consent.
type webSessionRegistry struct {
	mu          sync.Mutex
	tokens      map[[sha256.Size]byte]webSessionState
	uploads     map[*websocket.Conn]struct{}
	uploadGroup sync.WaitGroup
	revokeGroup sync.WaitGroup
	closing     bool
	onRevoke    func(string)
	login       *webLoginPolicy
}

type webSessionState struct {
	login        bool
	createdAt    time.Time
	expiresAt    time.Time
	expires      *time.Timer
	uploadSecret [sha256.Size]byte
	upload       *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
}

func newWebSessionRegistry(onRevoke ...func(string)) *webSessionRegistry {
	r := &webSessionRegistry{
		tokens:  make(map[[sha256.Size]byte]webSessionState),
		uploads: make(map[*websocket.Conn]struct{}),
	}
	if len(onRevoke) != 0 {
		r.onRevoke = onRevoke[0]
	}
	return r
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
	digest := sha256.Sum256([]byte(token))
	sessionCtx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		cancel()
		return "", "", errors.New("browser sessions are shutting down")
	}
	r.tokens[digest] = webSessionState{uploadSecret: uploadSecret, ctx: sessionCtx, cancel: cancel}
	r.mu.Unlock()
	return token, base64.RawURLEncoding.EncodeToString(uploadSecret[:]), nil
}

func (r *webSessionRegistry) authenticate(token string) (string, context.Context, bool) {
	if r == nil || token == "" {
		return "", nil, false
	}
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	state, ok := r.tokens[digest]
	if r.closing {
		ok = false
	}
	r.mu.Unlock()
	if !ok {
		return "", nil, false
	}
	return hex.EncodeToString(digest[:]), state.ctx, true
}

// withActiveOwner serializes a browser-owned publication with revocation.
// If publication wins, revoke's owner callback removes the new resource. If
// revocation wins, action is never called. The master credential does not use
// this browser-session fence.
func (r *webSessionRegistry) withActiveOwner(owner string, action func() error) (bool, error) {
	decoded, err := hex.DecodeString(owner)
	if err != nil || len(decoded) != sha256.Size {
		return false, nil
	}
	var digest [sha256.Size]byte
	copy(digest[:], decoded)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false, nil
	}
	if _, ok := r.tokens[digest]; !ok {
		return false, nil
	}
	return true, action()
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

func (r *webSessionRegistry) revoke(token string) bool {
	return r.revokeDigest(sha256.Sum256([]byte(token)))
}

func (r *webSessionRegistry) revokeDigest(digest [sha256.Size]byte) bool {
	r.mu.Lock()
	state, ok := r.tokens[digest]
	delete(r.tokens, digest)
	if ok && r.onRevoke != nil {
		// Register while holding mu so closeAll cannot begin waiting before an
		// expiry or logout callback has joined the drain group.
		r.revokeGroup.Add(1)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	state.cancel()
	if state.expires != nil {
		state.expires.Stop()
	}
	if state.upload != nil {
		_ = state.upload.CloseNow()
	}
	if r.onRevoke != nil {
		defer r.revokeGroup.Done()
		r.onRevoke(hex.EncodeToString(digest[:]))
	}
	return true
}

func (r *webSessionRegistry) closeAll(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	states := make(map[string]webSessionState, len(r.tokens))
	for digest, state := range r.tokens {
		state.cancel()
		if state.expires != nil {
			state.expires.Stop()
		}
		states[hex.EncodeToString(digest[:])] = state
	}
	if r.onRevoke != nil {
		r.revokeGroup.Add(len(states))
	}
	clear(r.tokens)
	conns := make([]*websocket.Conn, 0, len(r.uploads))
	for conn := range r.uploads {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.CloseNow()
	}
	if r.onRevoke != nil {
		for owner := range states {
			func() {
				defer r.revokeGroup.Done()
				r.onRevoke(owner)
			}()
		}
	}
	drained := make(chan struct{})
	go func() {
		r.uploadGroup.Wait()
		r.revokeGroup.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for browser upload handlers: %w", ctx.Err())
	}
}

// waitRevocations blocks until expiry, logout, and administrator revocation
// callbacks that already started have released their owner resources.
func (r *webSessionRegistry) waitRevocations() {
	r.revokeGroup.Wait()
}

func webSessionRequestAllowed(r *http.Request) bool {
	if photoFaceBrowserRequestAllowed(r) {
		return true
	}
	if photoAuthoredBrowserRequestAllowed(r) {
		return true
	}
	if photoAlbumBrowserRequestAllowed(r) {
		return true
	}
	if r.URL.Path == "/api/v1/photos/rejects/preflight" || r.URL.Path == "/api/v1/photos/rejects/trash" {
		return r.Method == http.MethodPost && r.URL.RawQuery == ""
	}
	if r.URL.Path == "/api/v1/photos/assets/query" || r.URL.Path == "/api/v1/photos/assets/timeline" || r.URL.Path == "/api/v1/photos/assets/flags" {
		return r.Method == http.MethodPost && r.URL.RawQuery == ""
	}
	if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/assets/"); ok {
		parts := strings.Split(after, "/")
		if len(parts) == 2 && parts[1] == "preview" {
			return r.Method == http.MethodPost && r.URL.RawQuery == "" && query.ValidateTextOperand("asset", parts[0]) == nil
		}
		if len(parts) == 3 && parts[1] == "previews" {
			values, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil {
				return false
			}
			for key, entries := range values {
				if key != "person_id" || len(entries) != 1 {
					return false
				}
			}
			if values.Has("person_id") && query.ValidateTextOperand("asset", values.Get("person_id")) != nil {
				return false
			}
			return r.Method == http.MethodGet && validPhotoPreviewPath(parts[0], parts[2])
		}
	}
	if emailNavigationBrowserReadAllowed(r) || emailViewerBrowserReadAllowed(r) {
		return true
	}
	method, path := r.Method, r.URL.Path
	if path == "/api/v1/search-exports" && method == http.MethodGet {
		query := r.URL.Query()
		if len(query) > 2 {
			return false
		}
		for key, values := range query {
			if (key != "offset" && key != webLimitQuery) || len(values) != 1 {
				return false
			}
			value, err := strconv.Atoi(values[0])
			if err != nil || value < 0 || value > 100 || key == webLimitQuery && value > 50 {
				return false
			}
		}
		return true
	}
	if r.URL.RawQuery == "" {
		if path == "/api/v1/search-exports" {
			return method == http.MethodPost
		}
		if after, ok := strings.CutPrefix(path, "/api/v1/search-exports/"); ok {
			parts := strings.Split(after, "/")
			if len(parts[0]) != 48 {
				return false
			}
			if _, err := hex.DecodeString(parts[0]); err != nil {
				return false
			}
			return len(parts) == 1 && method == http.MethodGet ||
				len(parts) == 2 && method == http.MethodPost &&
					(parts[1] == "dates" || parts[1] == "revisions" || parts[1] == "download")
		}
	}
	if strings.HasPrefix(path, "/api/v1/exports/") {
		return exportBrowserRouteAllowed(r)
	}
	if method == http.MethodPost && r.URL.RawQuery == "" &&
		(path == "/api/v1/workspace/queries" || isWorkspaceQueryPagePath(path) || isSavedQueryRunPath(path)) {
		return true
	}
	if path == "/api/v1/pages/inventory" || path == "/api/v1/pages/jobs" {
		return method == http.MethodPost && r.URL.RawQuery == ""
	}
	if after, ok := strings.CutPrefix(path, "/api/v1/pages/jobs/"); ok {
		parts := strings.Split(after, "/")
		return method == http.MethodPost && r.URL.RawQuery == "" && (len(parts) == 1 || len(parts) == 2 && parts[1] == "cancel") && validPageJobPathID(parts[0])
	}
	if path == "/api/v1/batch/tags" || path == "/api/v1/batch/tags/preview" {
		return method == http.MethodPost && r.URL.RawQuery == ""
	}
	if path == "/api/v1/queries/parse" || path == "/api/v1/queries/highlights" || path == "/api/v1/renditions/text" {
		return method == http.MethodPost && r.URL.RawQuery == ""
	}
	if r.URL.RawQuery == "" {
		if method == http.MethodPost && path == "/api/v1/email-pdfs" {
			return true
		}
		if method == http.MethodGet {
			if after, ok := strings.CutPrefix(path, "/api/v1/email-pdfs/"); ok && after != "" && len(strings.Split(after, "/")) <= 2 {
				return true
			}
			if after, ok := strings.CutPrefix(path, "/api/v1/email-pdf-jobs/"); ok && after != "" && !strings.Contains(after, "/") {
				return true
			}
		}
	}
	if mailboxBrowserRequestAllowed(r) {
		return true
	}
	if packagesBrowserRequestAllowed(r) {
		return true
	}
	if r.URL.RawQuery == "" {
		switch path {
		case photoCheckpointPath, "/api/v1/photos/settings":
			return method == http.MethodGet || method == http.MethodPut
		case photoCheckpointPath + "/header", "/api/v1/photos/hidden":
			return method == http.MethodGet
		case "/api/v1/photos/stacks/group", "/api/v1/photos/hidden/setup", "/api/v1/photos/hidden/change", "/api/v1/photos/hidden/disable", "/api/v1/photos/hidden/unlock", "/api/v1/photos/hidden/lock":
			return method == http.MethodPost
		}
		if after, ok := strings.CutPrefix(path, "/api/v1/photos/assets/"); ok {
			parts := strings.Split(after, "/")
			if len(parts) == 2 && query.ValidateTextOperand("asset", parts[0]) == nil && (parts[1] == "hide" || parts[1] == "unhide") {
				return method == http.MethodPost
			}
		}
	}
	if batesBrowserRequestAllowed(r) {
		return true
	}
	if lane, ok := strings.CutPrefix(path, "/api/v1/jobs/lanes/"); ok {
		return r.URL.RawQuery == "" && store.ControllableLane(lane) && method == http.MethodPut
	}
	if after, ok := strings.CutPrefix(path, "/api/v1/jobs/"); ok {
		parts := strings.Split(after, "/")
		return method == http.MethodPost && r.URL.RawQuery == "" && len(parts) == 2 && parts[1] == "cancel" && validPageJobPathID(parts[0])
	}
	if path == "/api/v1/saved-queries" {
		return method == http.MethodGet ||
			(method == http.MethodPost && r.URL.RawQuery == "")
	}
	if savedQueryID, ok := strings.CutPrefix(path, "/api/v1/saved-queries/"); ok &&
		savedQueryID != "" && !strings.Contains(savedQueryID, "/") {
		return method == http.MethodGet ||
			((method == http.MethodPatch || method == http.MethodDelete) &&
				r.URL.RawQuery == "")
	}
	if method == http.MethodPut && r.URL.RawQuery == "" {
		if collectionID, resource, ok := collectionResourcePath(path); ok &&
			collectionID != "" && resource == "label" {
			return true
		}
	}
	if method == http.MethodPost && path == webDownloadPreparePath && r.URL.RawQuery == "" {
		return true
	}
	if method == http.MethodDelete && path == webDownloadPreparePath {
		values, err := url.ParseQuery(r.URL.RawQuery)
		return err == nil && len(values) == 1 && len(values["ticket"]) == 1 &&
			values.Get("ticket") != ""
	}
	if method == http.MethodPost && path == "/api/v1/audit/verify" &&
		r.URL.RawQuery == "" {
		return true
	}
	if method == http.MethodPost && path == "/api/v1/tags" &&
		r.URL.RawQuery == "" {
		return true
	}
	if method == http.MethodPost && r.URL.RawQuery == "" {
		switch path {
		case "/api/v1/processing/plans", "/api/v1/processing/jobs",
			"/api/v1/processing/consent/grants", "/api/v1/processing/consent/revocations",
			"/api/v1/processing/source-fences/resolve", "/api/v1/search", "/api/v1/search/similar":
			return true
		}
	}
	if (method == http.MethodPatch || method == http.MethodDelete) &&
		r.URL.RawQuery == "" {
		if tagID, ok := strings.CutPrefix(path, "/api/v1/tags/"); ok &&
			tagID != "" && !strings.Contains(tagID, "/") {
			return true
		}
	}
	if method == http.MethodPost && r.URL.RawQuery == "" {
		if after, ok := strings.CutPrefix(path, "/api/v1/photos/assets/"); ok {
			parts := strings.Split(after, "/")
			if len(parts) == 2 && parts[0] != "" && parts[1] == "trash" {
				return true
			}
		}
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
	if (method == http.MethodPut || method == http.MethodDelete) &&
		r.URL.RawQuery == "" {
		const prefix = "/api/v1/nodes/"
		if after, ok := strings.CutPrefix(path, prefix); ok {
			parts := strings.Split(after, "/")
			nodeID, err := strconv.ParseInt(parts[0], 10, 64)
			if len(parts) == 3 && parts[1] == "tags" && parts[2] != "" &&
				err == nil && nodeID > 0 {
				return true
			}
		}
	}
	if method == http.MethodPost && path == telemetryEventsPath && r.URL.RawQuery == "" {
		return true
	}
	if method == http.MethodDelete && path == webSessionPath {
		return true
	}
	if method != http.MethodGet {
		return false
	}
	if after, ok := strings.CutPrefix(path, webMapTilePath+"/"); ok {
		return r.URL.RawQuery == "" && len(strings.Split(after, "/")) == 3
	}
	if path == "/api/v1/duplicates" {
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return false
		}
		for key, entries := range values {
			if (key != webLimitQuery && key != "offset") || len(entries) != 1 {
				return false
			}
		}
		return true
	}
	if path == "/api/v1/formats/capabilities" {
		return boundedFormatCoverageQuery(r.URL.RawQuery)
	}
	if path == "/api/v1/duplicates/by-hash" {
		values, err := url.ParseQuery(r.URL.RawQuery)
		return err == nil && len(values) == 2 && len(values["sha256"]) == 1 &&
			len(values["size"]) == 1 && values.Get("sha256") != "" && values.Get("size") != ""
	}
	if path == "/api/v1/renditions/text/content" {
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return false
		}
		allowed := map[string]bool{"node_id": true, "revision": true, "version_id": true,
			"blob_hash": true, "size": true, "profile_fingerprint": true, "generation_id": true,
			"attachment_id": true, "build_id": true, "artifact_id": true}
		for key, entries := range values {
			if !allowed[key] || len(entries) != 1 || entries[0] == "" {
				return false
			}
		}
		return len(values) >= 9 && len(values) <= 10
	}
	if path == "/api/v1/pages/image" {
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(values) != 9 {
			return false
		}
		for _, key := range []string{"node_id", "revision", "version_id", "source_sha256", "source_size", "page", "recipe_sha256", "frame_sha256", "image_sha256"} {
			if len(values[key]) != 1 || values.Get(key) == "" {
				return false
			}
		}
		return true
	}
	if path == "/api/v1/collections" {
		return true
	}
	if collectionID, resource, ok := collectionResourcePath(path); ok && collectionID != "" {
		return resource == "" || resource == "members" || resource == "label" || resource == "quality"
	}
	if path == "/api/v1/trash" {
		// The master API retains the released unbounded form, but a browser
		// session may request only the UI's fixed bounded page.
		return r.URL.RawQuery == "limit=1000&offset=0"
	}
	switch path {
	case "/api/v1/path", "/api/v1/search",
		"/api/v1/audit/status", "/api/v1/audit/history", "/api/v1/jobs",
		"/api/v1/storage", "/api/v1/tags", "/api/v1/processing/profiles",
		"/api/v1/coverage":
		return true
	case "/api/v1/backup/snapshots":
		// Browser sessions may inspect only the repository selected by daemon
		// configuration. An arbitrary repo query is a server-filesystem read
		// capability and remains exclusive to the master API credential.
		return r.URL.RawQuery == ""
	}
	for _, resourcePrefix := range []string{"/api/v1/processing/jobs/", "/api/v1/renditions/"} {
		if identity, ok := strings.CutPrefix(path, resourcePrefix); ok {
			return r.URL.RawQuery == "" && len(identity) == 64 &&
				strings.IndexFunc(identity, func(char rune) bool {
					return !strings.ContainsRune("0123456789abcdef", char)
				}) == -1
		}
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

func photoAlbumBrowserRequestAllowed(r *http.Request) bool {
	if r.URL.RawQuery != "" {
		return false
	}
	if r.URL.Path == "/api/v1/photos/albums" {
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	}
	after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/albums/")
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	if query.ValidateTextOperand("set", parts[0]) != nil {
		return false
	}
	if len(parts) == 1 {
		return r.Method == http.MethodPut || r.Method == http.MethodDelete
	}
	if len(parts) == 2 {
		return parts[1] == "cover" && r.Method == http.MethodPut || parts[1] == "duplicate" && r.Method == http.MethodPost
	}
	return len(parts) == 3 && parts[1] == "members" && (parts[2] == "add" || parts[2] == "remove") && r.Method == http.MethodPost
}

func boundedFormatCoverageQuery(rawQuery string) bool {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	for key, entries := range values {
		if (key != "family" && key != "format" && key != "extension") || len(entries) != 1 {
			return false
		}
	}
	return validateFormatQuery(values.Get("family"), values.Get("format"), values.Get("extension")) == nil
}

func isWorkspaceQueryPagePath(path string) bool {
	const prefix = "/api/v1/workspace/queries/"
	after, ok := strings.CutPrefix(path, prefix)
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == "pages"
}

func isSavedQueryRunPath(path string) bool {
	const prefix = "/api/v1/saved-queries/"
	after, ok := strings.CutPrefix(path, prefix)
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == "runs"
}

func collectionResourcePath(path string) (collectionID, resource string, ok bool) {
	const prefix = "/api/v1/collections/"
	after, ok := strings.CutPrefix(path, prefix)
	if !ok {
		return "", "", false
	}
	parts := strings.Split(after, "/")
	if len(parts) == 1 {
		return parts[0], "", true
	}
	if len(parts) == 2 && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func registerWebSession(
	mux *http.ServeMux,
	enabled bool,
	webURL string,
	sessions *webSessionRegistry,
	capture telemetry.CaptureEndpoint,
	logger *slog.Logger,
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
		token := r.Header.Get(WebSessionHeader)
		live := sessions.revoke(token)
		values, err := url.ParseQuery(r.URL.RawQuery)
		bucket := values.Get("duration_bucket")
		if live && err == nil && len(values["duration_bucket"]) == 1 && slices.Contains(telemetry.DurationBuckets(), bucket) && capture != nil {
			if err := capture.Capture(telemetry.EventSessionEnded, map[string]any{"surface": "web", "duration_bucket": bucket}); err != nil {
				logger.DebugContext(r.Context(), "final session_ended capture failed", "error", err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func validPhotoPreviewPath(assetID, generationID string) bool {
	if query.ValidateTextOperand("asset", assetID) != nil || len(generationID) != 64 {
		return false
	}
	_, err := hex.DecodeString(generationID)
	return err == nil && strings.ToLower(generationID) == generationID
}

func photoFaceBrowserRequestAllowed(r *http.Request) bool {
	if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/people/"); ok {
		parts := strings.Split(after, "/")
		if r.Method != http.MethodGet || len(parts) != 2 || parts[1] != "face-previews" || query.ValidateTextOperand("asset", parts[0]) != nil {
			return false
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return false
		}
		for key, entries := range values {
			if key != "hidden" || len(entries) != 1 {
				return false
			}
		}
		return true
	}
	if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/people/by-id/"); ok {
		parts := strings.Split(after, "/")
		if query.ValidateTextOperand("asset", parts[0]) != nil || r.URL.RawQuery != "" {
			return false
		}
		return len(parts) == 1 && r.Method == http.MethodPatch || len(parts) == 2 && parts[1] == "merge" && r.Method == http.MethodPost
	}
	if r.URL.Path == "/api/v1/people" {
		if r.Method != http.MethodGet {
			return false
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return false
		}
		for key, entries := range values {
			if (key != "query" && key != "limit" && key != "cursor" && key != "photo_summary" && key != "recent_first" && key != "person_id") || len(entries) != 1 {
				return false
			}
		}
		return true
	}
	if after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/photos/versions/"); ok {
		parts := strings.Split(after, "/")
		if len(parts) != 2 || query.ValidateTextOperand("asset", parts[0]) != nil || r.URL.RawQuery != "" {
			return false
		}
		return parts[1] == "faces" && (r.Method == http.MethodGet || r.Method == http.MethodPost) || parts[1] == "face-decisions" && r.Method == http.MethodPost
	}
	return false
}
