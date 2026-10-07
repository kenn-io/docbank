package telemetry

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
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
	claims   screenClaims
}

// CaptureHandler tracks daily claims for each interface under the daemon's vault lock.
func CaptureHandler(r *Reporter, dir string) http.Handler {
	return &screenCapture{reporter: r, next: posthog.NewCaptureHandler(r), dir: dir, now: time.Now}
}

func (h *screenCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method != http.MethodPost || err != nil || media != "application/json" {
		h.next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))

	var request struct {
		Event      string
		Properties map[string]any
	}
	if err != nil || len(body) > 64<<10 {
		h.next.ServeHTTP(w, r)
		return
	}
	// Options follow kit's v1 decoder so both sides agree on which body names the event.
	decoder := jsontext.NewDecoder(bytes.NewReader(body), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	token, err := decoder.ReadToken()
	if err != nil || token.Kind() != '{' {
		h.next.ServeHTTP(w, r)
		return
	}
	for err == nil && decoder.PeekKind() != '}' {
		token, err = decoder.ReadToken()
		if err != nil {
			break
		}
		switch {
		case strings.EqualFold(token.String(), "event"):
			if decoder.PeekKind() == 'n' {
				_, err = decoder.ReadToken()
			} else {
				err = json.UnmarshalDecode(decoder, &request.Event)
			}
		case strings.EqualFold(token.String(), "properties"):
			err = json.UnmarshalDecode(decoder, &request.Properties)
		default:
			err = decoder.SkipValue()
		}
	}
	if err == nil {
		_, err = decoder.ReadToken()
	}
	if err != nil || !trailingEOF(decoder) || strings.TrimSpace(request.Event) != EventScreenViewed || !h.reporter.Enabled() {
		h.next.ServeHTTP(w, r)
		return
	}
	properties, _ := h.reporter.SanitizeProperties(EventScreenViewed, request.Properties)
	screen, validScreen := properties["screen"].(string)
	surface, validSurface := properties["surface"].(string)
	if !validScreen || !validSurface {
		screenReceipt(w, "queued")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.claims.Day != h.now().UTC().Format(time.DateOnly) {
		h.claims = h.load()
	}
	key := screen + "|" + surface
	if h.claims.Screens[key] {
		screenReceipt(w, "queued")
		return
	}
	if err := h.reporter.Capture(EventScreenViewed, properties); err != nil {
		http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
		return
	}
	if !h.reporter.Enabled() {
		screenReceipt(w, "disabled")
		return
	}
	h.claims.Screens[key] = true
	if err := h.save(h.claims); err != nil {
		slog.Warn("telemetry screen claims failed", "error", err)
	}
	screenReceipt(w, "queued")
}

func (h *screenCapture) load() screenClaims {
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
	body, err := io.ReadAll(io.LimitReader(file, 8193))
	if err == nil && len(body) > 8192 {
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

func (h *screenCapture) save(claims screenClaims) error {
	data, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(h.dir, screenClaimsFile), data, atomicfile.WithPrivate())
}

func screenReceipt(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"status":"`+status+`"}`)
}

func trailingEOF(decoder *jsontext.Decoder) bool {
	_, err := decoder.ReadValue()
	return errors.Is(err, io.EOF)
}
