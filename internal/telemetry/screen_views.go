package telemetry

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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

// Capture shares validation and daily claims between daemon and HTTP callers.
type Capture struct {
	reporter *Reporter
	dir      string
	mu       sync.Mutex
	now      func() time.Time
	claims   screenClaims

	importsMu sync.Mutex
	imports   map[string]string // import ID to admitting surface; "" once claimed
}

// CaptureHandler serves interface events with shared validation and claims.
func CaptureHandler(r *Reporter, dir string) *Capture {
	return &Capture{reporter: r, dir: dir, now: time.Now}
}

func (h *Capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeCaptureRequest(w, r)
	if !ok {
		return
	}
	// Only the daemon confirms additions, after it commits them.
	if strings.TrimSpace(request.Event) == EventDocumentAdded {
		http.Error(w, posthog.ErrUnsupportedEvent.Error(), http.StatusBadRequest)
		return
	}
	if err := h.Capture(request.Event, request.Properties); err != nil {
		if errors.Is(err, posthog.ErrInvalidProperty) || errors.Is(err, posthog.ErrUnsupportedEvent) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
		}
		return
	}
	status := "queued"
	if !h.reporter.Enabled() {
		status = "disabled"
	}
	writeCaptureStatus(w, status)
}

// Capture validates and claims interface events for HTTP and daemon callers.
func (h *Capture) Capture(event string, properties map[string]any) error {
	event = strings.TrimSpace(event)
	if event == EventScreenViewed || event == EventSearchRun || event == EventDocumentOpened {
		// Daily claims key on sanitized values.
		props, err := h.reporter.SanitizeProperties(event, properties)
		if err != nil {
			return fmt.Errorf("validate telemetry properties: %w", err)
		}
		return h.captureDaily(event, props)
	}
	ctx, cancel := context.WithTimeout(context.Background(), posthog.ShutdownTimeout)
	defer cancel()
	if _, err := h.reporter.Report(ctx, event, properties); err != nil {
		return fmt.Errorf("capture telemetry event: %w", err)
	}
	return nil
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

func (h *Capture) captureDaily(event string, properties map[string]any) error {
	if !h.reporter.Enabled() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.claims.Day != h.now().UTC().Format(time.DateOnly) {
		h.claims = h.load()
	}
	key := event + "|" + fmt.Sprint(properties["surface"])
	if event == EventScreenViewed {
		key = fmt.Sprint(properties["screen"]) + "|" + fmt.Sprint(properties["surface"])
	}
	if event == EventSearchRun {
		key += "|" + fmt.Sprint(properties["search_type"])
	}
	if h.claims.Screens[key] {
		return nil
	}
	if err := h.reporter.Capture(event, properties); err != nil {
		return fmt.Errorf("capture daily telemetry event: %w", err)
	}
	if !h.reporter.Enabled() {
		return nil
	}
	h.claims.Screens[key] = true
	if err := h.save(h.claims); err != nil {
		slog.Warn("telemetry screen claims failed", "error", err)
	}
	return nil
}

// load reads today's claims for the current install ID. The reporter is
// enabled here, so New has already created the install file.
func (h *Capture) load() screenClaims {
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

func (h *Capture) save(claims screenClaims) error {
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
