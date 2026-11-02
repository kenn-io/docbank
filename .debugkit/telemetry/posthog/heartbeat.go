package posthog

import (
	"context"
	"log/slog"
	"time"
)

const (
	// EventDaemonActive is the event RunHeartbeat sends.
	EventDaemonActive = "daemon_active"
	// HeartbeatInterval is how often RunHeartbeat checks the current UTC date.
	HeartbeatInterval = time.Hour
)

// RunHeartbeat sends EventDaemonActive through an enabled client at once, then
// on the first hourly check of a later UTC day until ctx ends or client becomes
// disabled. A disabled client starts no ticker. The client must allow the event.
// Capture errors are logged to logger, or slog.Default when logger is nil.
func RunHeartbeat(ctx context.Context, client Client, logger *slog.Logger) {
	if !client.Enabled() {
		return
	}
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	runHeartbeat(ctx, client, ticker.C, time.Now, logger)
}

func runHeartbeat(ctx context.Context, client Client, ticks <-chan time.Time, now func() time.Time, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	var lastDay time.Time
	for ctx.Err() == nil {
		if !client.Enabled() {
			return
		}
		// Read wall time afresh: a ticker may carry an old timestamp after sleep.
		day := now().UTC().Truncate(24 * time.Hour)
		if day.After(lastDay) {
			if err := client.Capture(EventDaemonActive, nil); err != nil {
				logger.Warn("telemetry heartbeat failed", "event", EventDaemonActive, "error", err)
			}
			lastDay = day
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}
