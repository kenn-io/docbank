package telemetry

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/kit/telemetry/posthog"
)

var screenNames = []string{"browse", "search", "tags", "snapshot", "history", "versions", "provenance", "jobs", "audit_evidence", "storage", "backups", "bates", "export", "saved_queries", "collections", "trash", "tag_catalog", "telemetry", "term_reports", "processing", "rendition", "upload", "mailbox", "load_file", "snapshot_actions", "help", "document", "packages", "operations"}

const screenClaimsFile = "telemetry-screen-views.json"

type screenClaims struct {
	InstallID string          `json:"install_id"`
	Day       string          `json:"day"`
	Screens   map[string]bool `json:"screens"`
}

type screenCapture struct {
	reporter *Reporter
	next     http.Handler
	dir      string
	mu       sync.Mutex
	now      func() time.Time
}

// CaptureHandler shares daily claims across interfaces under the daemon's vault lock.
func CaptureHandler(r *Reporter, dir string) http.Handler {
	return &screenCapture{reporter: r, next: posthog.NewCaptureHandler(r), dir: dir, now: time.Now}
}

func (h *screenCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		h.next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))

	var request struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	if err != nil || len(body) > 64<<10 {
		h.next.ServeHTTP(w, r)
		return
	}
	if err := json.Unmarshal(body, &request, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true), json.MatchCaseInsensitiveNames(true)); err != nil {
		http.Error(w, "invalid telemetry request", http.StatusBadRequest)
		return
	}
	request.Event = strings.TrimSpace(request.Event)
	if request.Event == EventScreenViewed {
		request.Properties, _ = h.reporter.SanitizeProperties(request.Event, request.Properties)
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		http.Error(w, "invalid telemetry properties", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(canonical))
	if request.Event != EventScreenViewed || !h.reporter.EventAllowed(request.Event) {
		h.next.ServeHTTP(w, r)
		return
	}
	properties := request.Properties
	status := "queued"
	if !h.reporter.Enabled() {
		status = "disabled"
	}
	screen, validScreen := properties["screen"].(string)
	_, validSurface := properties["surface"].(string)
	if !validScreen || !validSurface || status == "disabled" {
		screenReceipt(w, status)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	claims, err := h.load()
	if err != nil {
		h.storageError(w, err)
		return
	}
	if claims.Screens[screen] {
		screenReceipt(w, "queued")
		return
	}
	claims.Screens[screen] = true
	if err := h.save(claims); err != nil {
		h.storageError(w, err)
		return
	}
	response := &screenResponse{header: make(http.Header)}
	h.next.ServeHTTP(response, r)
	if response.code != http.StatusAccepted {
		delete(claims.Screens, screen)
		if err := h.save(claims); err != nil {
			h.storageError(w, err)
			return
		}
	}
	maps.Copy(w.Header(), response.header)
	w.WriteHeader(response.code)
	_, _ = w.Write(response.body.Bytes())
}

func (h *screenCapture) load() (screenClaims, error) {
	inst, err := posthog.LoadOrCreateInstall(h.dir)
	if err != nil {
		return screenClaims{}, fmt.Errorf("load telemetry identity: %w", err)
	}
	claims := screenClaims{}
	file, err := os.Open(filepath.Join(h.dir, screenClaimsFile))
	if err == nil {
		defer func() { _ = file.Close() }()
		body, readErr := io.ReadAll(io.LimitReader(file, 8193))
		if readErr != nil {
			return claims, readErr
		}
		if len(body) > 8192 {
			return claims, errors.New("telemetry screen claims too large")
		}
		if err := json.Unmarshal(body, &claims); err != nil {
			return claims, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return claims, err
	}
	day := h.now().UTC().Format(time.DateOnly)
	if claims.InstallID != inst.ID || claims.Day != day {
		claims = screenClaims{InstallID: inst.ID, Day: day}
	}
	if claims.Screens == nil {
		claims.Screens = map[string]bool{}
	}
	return claims, nil
}

func (h *screenCapture) save(claims screenClaims) error {
	stage, err := filepublish.CreateStage(h.dir, ".telemetry-screens-")
	if err != nil {
		return err
	}
	defer func() { _ = stage.Cleanup() }()
	if err := json.MarshalWrite(stage.File, claims); err != nil {
		return err
	}
	if err := stage.File.Sync(); err != nil {
		return err
	}
	if err := stage.File.Close(); err != nil {
		return err
	}
	committed, err := filepublish.Publish(stage.Path(), filepath.Join(h.dir, screenClaimsFile), true)
	if committed && err != nil {
		slog.Warn("telemetry screen claim durability failed", "error", err)
		return nil
	}
	return err
}

func (h *screenCapture) storageError(w http.ResponseWriter, err error) {
	slog.Warn("telemetry screen claims failed", "error", err)
	http.Error(w, "persist telemetry screen claim failed", http.StatusInternalServerError)
}

func screenReceipt(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"status":"`+status+`"}`)
}

type screenResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *screenResponse) Header() http.Header  { return w.header }
func (w *screenResponse) WriteHeader(code int) { w.code = code }
func (w *screenResponse) Write(body []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, _ := w.body.Write(body)
	return n, nil
}
