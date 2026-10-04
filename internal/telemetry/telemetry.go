// Package telemetry reports anonymous daemon and web app usage events.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"

	"go.kenn.io/kit/telemetry/posthog"
)

const (
	// EnabledEnv set to "0" turns off docbank's anonymous usage telemetry.
	EnabledEnv         = "DOCBANK_TELEMETRY_ENABLED"
	EventDaemonStarted = "daemon_started"
	EventDaemonActive  = posthog.EventDaemonActive
	EventAppOpened     = "app_opened"
	// HeartbeatJobName is the supervised job that sends daemon_started and daemon_active.
	HeartbeatJobName = "telemetry:heartbeat"

	application = "docbank"
	envPrefix   = "DOCBANK"
	// PostHog project API keys are public ingest identifiers, not credentials.
	postHogAPIKey = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf" // #nosec G101
)

// Reporter is kit's PostHog reporter configured for docbank.
type Reporter = posthog.Reporter

// Options configures New.
type Options struct {
	Dir      string // vault root; holds kit's install file
	Version  string
	Commit   string
	Logger   *slog.Logger // nil uses slog.Default()
	endpoint string       // tests point the enabled reporter at a loopback server
}

// New returns docbank's reporter. It never fails: opted out, it keeps the
// event allowlist and sends nothing; when the install ID can't be read or
// created, it admits no event.
func New(opts Options) *Reporter {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var inst posthog.Install
	if posthog.EnabledFromEnv(envPrefix) {
		var err error
		if inst, err = posthog.LoadOrCreateInstall(opts.Dir); err != nil {
			logger.Warn("telemetry disabled", "error", err)
			return posthog.DisabledReporter()
		}
	}
	reporter, err := posthog.NewReporter(posthog.Options{
		Logger:      logger,
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
		posthog.WithAllowedEvent(EventDaemonStarted),
		posthog.WithAllowedEvent(EventDaemonActive),
		posthog.WithAllowedEvent(EventAppOpened),
	)
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return posthog.DisabledReporter()
	}
	return reporter
}

// CaptureHandler serves the web app's usage-event posts through r.
func CaptureHandler(r *Reporter) http.Handler { return posthog.NewCaptureHandler(r) }

// RunHeartbeat sends daemon_started, then kit's daemon_active heartbeat until ctx ends.
func RunHeartbeat(ctx context.Context, r posthog.Client, logger *slog.Logger) {
	if err := r.Capture(EventDaemonStarted, nil); err != nil {
		logger.Warn("telemetry capture failed", "event", EventDaemonStarted, "error", err)
	}
	posthog.RunHeartbeat(ctx, r, logger)
}
