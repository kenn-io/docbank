// Package telemetry reports anonymous daemon and web app usage events.
package telemetry

import (
	"log/slog"
	"time"

	"go.kenn.io/kit/telemetry/posthog"
)

const (
	// EnabledEnv set to "0" turns off docbank's anonymous usage telemetry.
	EnabledEnv          = "DOCBANK_TELEMETRY_ENABLED"
	EventDaemonActive   = posthog.EventDaemonActive
	EventAppOpened      = "app_opened"
	EventScreenViewed   = "screen_viewed"
	EventSessionEnded   = "session_ended"
	EventDocumentAdded  = "document_added"
	EventSearchRun      = "search_run"
	EventDocumentOpened = "document_opened"
	// HeartbeatJobName is the supervised job that sends daemon_active.
	HeartbeatJobName = "telemetry:heartbeat"

	durationUnderMinute         = "under_1m"
	durationOneToFiveMinutes    = "1_to_5m"
	durationFiveToThirtyMinutes = "5_to_30m"
	durationOverThirtyMinutes   = "over_30m"

	application = "docbank"
	envPrefix   = "DOCBANK"
)

// Reporter is kit's PostHog reporter configured for docbank.
type Reporter = posthog.Reporter

// Options configures New.
type Options struct {
	Dir      string // vault root; holds kit's install file
	Version  string
	Commit   string
	Logger   *slog.Logger // nil uses slog.Default()
	apiKey   string       // empty uses the key release builds set at link time
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
	surface := posthog.RequireProperty("surface", posthog.AllowStringValues("web", "tui", "cli"))
	reporter, err := posthog.NewReporter(posthog.Options{
		Logger:      logger,
		APIKey:      opts.apiKey,
		Endpoint:    opts.endpoint,
		Application: application,
		EnvPrefix:   envPrefix,
		DistinctID:  inst.ID,
		InstalledAt: inst.InstalledAt,
		Version:     opts.Version,
		Commit:      opts.Commit,
		Source:      "daemon",
	},
		posthog.WithAllowedEvent(EventDaemonActive),
		posthog.WithAllowedEvent(EventAppOpened),
		posthog.WithAllowedEvent(EventDocumentAdded, surface),
		posthog.WithAllowedEvent(EventDocumentOpened, surface),
		posthog.WithAllowedEvent(EventSearchRun,
			surface,
			posthog.RequireProperty("search_type", posthog.AllowStringValues("text", "semantic", "hybrid"))),
		posthog.WithAllowedEvent(EventScreenViewed,
			posthog.AllowProperty("screen", posthog.AllowStringValues(screenNames...)),
			posthog.AllowProperty("surface", posthog.AllowStringValues("web", "tui"))),
		posthog.WithAllowedEvent(EventSessionEnded,
			posthog.AllowProperty("surface", posthog.AllowStringValues("web", "tui")),
			posthog.AllowProperty("duration_bucket", posthog.AllowStringValues(DurationBuckets()...)),
		),
	)
	if err != nil {
		logger.Warn("telemetry disabled", "error", err)
		return posthog.DisabledReporter()
	}
	return reporter
}

// DurationBuckets returns the allowed session duration values in ascending order.
func DurationBuckets() []string {
	return []string{durationUnderMinute, durationOneToFiveMinutes, durationFiveToThirtyMinutes, durationOverThirtyMinutes}
}

// DurationBucket groups elapsed time without sending an exact duration.
func DurationBucket(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return durationUnderMinute
	case elapsed < 5*time.Minute:
		return durationOneToFiveMinutes
	case elapsed <= 30*time.Minute:
		return durationFiveToThirtyMinutes
	default:
		return durationOverThirtyMinutes
	}
}

// SearchType names the executed search mode in anonymous action events.
func SearchType(actualMode string) string {
	if actualMode == "lexical" {
		return "text"
	}
	return actualMode
}
