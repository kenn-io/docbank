// Package telemetry reports anonymous daemon and web app usage events.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kit/atomicfile"
	kittelemetry "go.kenn.io/kit/telemetry"
)

const (
	// EnabledEnv set to "0" turns off docbank's anonymous usage telemetry.
	EnabledEnv         = "DOCBANK_TELEMETRY_ENABLED"
	EventDaemonStarted = "daemon_started"
	EventDaemonActive  = "daemon_active"
	EventAppOpened     = "app_opened"
	// HeartbeatJobName is the supervised job that sends daemon_started and daemon_active.
	HeartbeatJobName = "telemetry:heartbeat"
	// CloseTimeout bounds the final flush so shutdown stays inside daemon.GracefulExitTimeout.
	CloseTimeout = 2 * time.Second

	application       = "docbank"
	envPrefix         = "DOCBANK"
	heartbeatInterval = 24 * time.Hour
	// PostHog project API keys are public ingest identifiers, not credentials.
	postHogAPIKey = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf" // #nosec G101
)

// Reporter is kit's PostHog reporter configured for docbank.
type Reporter = kittelemetry.PostHogReporter

// Options configures New.
type Options struct {
	InstallPath string // home.Layout.TelemetryInstallPath()
	Version     string
	Commit      string
	Logger      *slog.Logger // nil uses slog.Default()
	endpoint    string       // tests point the enabled reporter at a loopback server
}

type install struct {
	ID          string    `json:"install_id"`
	InstalledAt time.Time `json:"installed_at"`
}

// New returns docbank's reporter. It never fails: opted out, it keeps the
// event allowlist and sends nothing; under go test or when the install ID
// cannot be read or created, it admits no event.
func New(opts Options) *Reporter {
	logger := loggerOf(opts)
	if !kittelemetry.PostHogTelemetryEnabledFromEnv(envPrefix) {
		reporter, err := newKitReporter(opts, install{})
		if err != nil {
			logger.Warn("telemetry disabled", "error", err)
			return kittelemetry.DisabledPostHogReporter()
		}
		return reporter
	}
	if testing.Testing() {
		return kittelemetry.DisabledPostHogReporter()
	}
	return newEnabled(opts)
}

func newEnabled(opts Options) *Reporter {
	logger := loggerOf(opts)
	inst, err := loadOrCreateInstall(opts.InstallPath, time.Now())
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return kittelemetry.DisabledPostHogReporter()
	}
	reporter, err := newKitReporter(opts, inst)
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return kittelemetry.DisabledPostHogReporter()
	}
	return reporter
}

func newKitReporter(opts Options, inst install) (*Reporter, error) {
	reporter, err := kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      postHogAPIKey,
		Endpoint:    opts.endpoint,
		Application: application,
		EnvPrefix:   envPrefix,
		DistinctID:  inst.ID,
		InstalledAt: inst.InstalledAt,
		Version:     opts.Version,
		Commit:      opts.Commit,
		Source:      "daemon",
	},
		kittelemetry.WithAllowedEvent(EventDaemonStarted),
		kittelemetry.WithAllowedEvent(EventDaemonActive),
		kittelemetry.WithAllowedEvent(EventAppOpened),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry reporter: %w", err)
	}
	return reporter, nil
}

func loggerOf(opts Options) *slog.Logger {
	if opts.Logger != nil {
		return opts.Logger
	}
	return slog.Default()
}

// loadOrCreateInstall reads the install ID at path, creating it once when absent.
// An unreadable or malformed file is reported and left as it is.
func loadOrCreateInstall(path string, now time.Time) (install, error) {
	inst, err := readInstall(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return inst, err
	}
	inst = install{ID: rand.Text(), InstalledAt: now.UTC()}
	data, err := json.Marshal(inst)
	if err != nil {
		return install{}, err
	}
	if err := atomicfile.WriteNew(path, data, atomicfile.WithPrivate()); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readInstall(path)
		}
		return install{}, fmt.Errorf("create telemetry install file: %w", err)
	}
	return inst, nil
}

func readInstall(path string) (install, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return install{}, err
	}
	var inst install
	if err := json.Unmarshal(data, &inst); err != nil {
		return install{}, fmt.Errorf("read telemetry install file: %w", err)
	}
	inst.ID = strings.TrimSpace(inst.ID)
	if inst.ID == "" {
		return install{}, errors.New("telemetry install file has no install_id")
	}
	return inst, nil
}

// CaptureHandler serves the web app's usage-event posts through r.
func CaptureHandler(r *Reporter) http.Handler { return kittelemetry.NewPostHogCaptureHandler(r) }

// RunHeartbeat sends daemon_started and daemon_active now, then daemon_active
// every 24 hours until ctx ends.
func RunHeartbeat(ctx context.Context, r *Reporter, logger *slog.Logger) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	runHeartbeat(ctx, func(event string) error { return r.Capture(event, nil) }, ticker.C, logger)
}

func runHeartbeat(ctx context.Context, capture func(event string) error, ticks <-chan time.Time, logger *slog.Logger) {
	send := func(event string) {
		if err := capture(event); err != nil {
			logger.Warn("telemetry capture failed", "event", event, "error", err)
		}
	}
	send(EventDaemonStarted)
	send(EventDaemonActive)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			send(EventDaemonActive)
		}
	}
}

// CloseWithin flushes c but stops waiting after timeout, so a slow or
// unreachable endpoint cannot hold up daemon shutdown.
func CloseWithin(c io.Closer, timeout time.Duration, logger *slog.Logger) {
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			logger.Warn("telemetry close failed", "error", err)
		}
	case <-timer.C:
		logger.Warn("telemetry flush abandoned at shutdown", "timeout", timeout)
	}
}
