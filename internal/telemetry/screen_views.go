package telemetry

import (
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/telemetry/posthog"
)

var screenNames = []string{"browse", "search", "tags", "snapshot", "history", "versions", "provenance", "jobs", "audit_evidence", "storage", "backups", "bates", "export", "saved_queries", "collections", "trash", "tag_catalog", "telemetry", "term_reports", "processing", "rendition", "upload", "mailbox", "load_file", "snapshot_actions", "help", "document", "packages", "operations"}

// ScreenNames returns the screen values screen_viewed accepts.
func ScreenNames() []string { return slices.Clone(screenNames) }

const (
	screenClaimsFile     = "telemetry-screen-views.json"
	maxCaptureBodyBytes  = 64 << 10
	maxScreenClaimsBytes = 8 << 10
)

type captureRequest struct {
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
}

type screenClaims struct {
	InstallID string          `json:"install_id"`
	Day       string          `json:"day"`
	Screens   map[string]bool `json:"screens"`
}

type captureHandler struct {
	reporter *Reporter
	dir      string
	mu       sync.Mutex
	now      func() time.Time
	claims   screenClaims
}

// CaptureHandler serves interface usage events through r. It sends each
// screen_viewed screen and surface pair once per UTC day, remembering the day's
// claims in dir so daemon restarts don't resend them.
//
// Responses: 202 {"status":"queued"} when the event is queued or was already
// sent today; 202 {"status":"disabled"} when telemetry is off; 400 for a
// malformed body, an event the allowlist omits, or a screen_viewed without an
// allowed screen and surface; 413 for a body over 64 KiB; 415 for a content
// type other than application/json; 500 when Capture fails.
func CaptureHandler(r *Reporter, dir string) http.Handler {
	return &captureHandler{reporter: r, dir: dir, now: time.Now}
}

func (h *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeCaptureRequest(w, r)
	if !ok {
		return
	}
	event := strings.TrimSpace(request.Event)
	if !h.reporter.EventAllowed(event) {
		http.Error(w, posthog.ErrUnsupportedEvent.Error(), http.StatusBadRequest)
		return
	}
	if event == EventScreenViewed {
		h.serveScreen(w, request.Properties)
		return
	}
	if !h.reporter.Enabled() {
		writeCaptureStatus(w, "disabled")
		return
	}
	if err := h.reporter.Capture(event, request.Properties); err != nil {
		http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
		return
	}
	writeCaptureStatus(w, "queued")
}

func decodeCaptureRequest(w http.ResponseWriter, r *http.Request) (captureRequest, bool) {
	var request captureRequest
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		http.Error(w, "telemetry request must be application/json", http.StatusUnsupportedMediaType)
		return request, false
	}
	err = json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxCaptureBodyBytes), &request)
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		http.Error(w, "telemetry request too large", http.StatusRequestEntityTooLarge)
		return request, false
	}
	if err != nil {
		http.Error(w, "invalid telemetry request", http.StatusBadRequest)
		return request, false
	}
	return request, true
}

func (h *captureHandler) serveScreen(w http.ResponseWriter, raw map[string]any) {
	properties, err := h.reporter.SanitizeProperties(EventScreenViewed, raw)
	screen, validScreen := properties["screen"].(string)
	surface, validSurface := properties["surface"].(string)
	if err != nil || !validScreen || !validSurface {
		http.Error(w, "screen_viewed requires an allowed screen and surface", http.StatusBadRequest)
		return
	}
	if !h.reporter.Enabled() {
		writeCaptureStatus(w, "disabled")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.claims.Day != h.now().UTC().Format(time.DateOnly) {
		h.claims = h.load()
	}
	key := screen + "|" + surface
	if h.claims.Screens[key] {
		writeCaptureStatus(w, "queued")
		return
	}
	if err := h.reporter.Capture(EventScreenViewed, properties); err != nil {
		http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
		return
	}
	if !h.reporter.Enabled() {
		writeCaptureStatus(w, "disabled")
		return
	}
	h.claims.Screens[key] = true
	if err := h.save(h.claims); err != nil {
		slog.Warn("telemetry screen claims failed", "error", err)
	}
	writeCaptureStatus(w, "queued")
}

// load reads today's claims for the current install ID. The reporter is
// enabled here, so New has already created the install file.
func (h *captureHandler) load() screenClaims {
	claims := screenClaims{Day: h.now().UTC().Format(time.DateOnly), Screens: map[string]bool{}}
	inst, err := posthog.LoadOrCreateInstall(h.dir)
	if err != nil {
		slog.Warn("load telemetry identity failed", "error", err)
		return claims
	}
	claims.InstallID = inst.ID
	file, err := os.Open(filepath.Join(h.dir, screenClaimsFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("load telemetry screen claims failed", "error", err)
		}
		return claims
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxScreenClaimsBytes+1))
	if err == nil && len(body) > maxScreenClaimsBytes {
		err = errors.New("telemetry screen claims too large")
	}
	var stored screenClaims
	if err == nil {
		err = json.Unmarshal(body, &stored)
	}
	if err != nil {
		slog.Warn("load telemetry screen claims failed", "error", err)
		return claims
	}
	if stored.InstallID == claims.InstallID && stored.Day == claims.Day && stored.Screens != nil {
		return stored
	}
	return claims
}

func (h *captureHandler) save(claims screenClaims) error {
	data, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(h.dir, screenClaimsFile), data, atomicfile.WithPrivate())
}

func writeCaptureStatus(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"status":"`+status+`"}`)
}
