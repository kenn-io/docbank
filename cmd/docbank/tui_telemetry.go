package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/telemetry"
)

// tuiTelemetry remembers daemon opt-out receipts and bounds reports within a TUI run.
type tuiTelemetry struct {
	mu       sync.Mutex
	day      string
	disabled bool
	screens  map[string]bool
	failures int
	running  func(context.Context) (*daemonconn.Connection, bool, error)
}

func (r *tuiTelemetry) report(ctx context.Context, body *apiclient.ReportTelemetryEventBody) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	day := time.Now().UTC().Format(time.DateOnly)
	if r.day != day {
		r.day, r.disabled, r.failures = day, false, 0
		r.screens = make(map[string]bool)
	}
	screen := body.Properties.Screen
	if r.disabled || r.screens[screen] || r.failures >= 6 {
		return nil
	}
	layout, err := home.Resolve()
	if err != nil {
		return fmt.Errorf("resolving telemetry state: %w", err)
	}
	path := filepath.Join(layout.Root, "telemetry-tui-disabled")
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("locking TUI telemetry: %w", err)
	}
	if !locked {
		return errors.New("locking TUI telemetry: lock unavailable")
	}
	defer func() { _ = lock.Close() }()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading TUI telemetry receipt: %w", err)
	}
	if string(data) == day {
		r.disabled = true
		return nil
	}
	running := r.running
	if running == nil {
		running = daemonconn.Running
	}
	c, ok, err := running(ctx)
	if err != nil {
		return fmt.Errorf("finding daemon for telemetry: %w", err)
	}
	if !ok {
		return errors.New("reporting TUI telemetry: daemon is not running")
	}
	defer func() { _ = c.Close() }()
	receipt, err := c.API().ReportTelemetryEvent(ctx, &apiclient.ReportTelemetryEventRequestOptions{Body: body})
	if err != nil {
		r.failures++
		return fmt.Errorf("reporting TUI telemetry: %w", err)
	}
	switch receipt.Status {
	case "disabled":
		r.disabled = true
		if err := atomicfile.WriteFile(path, []byte(day), atomicfile.WithPrivate()); err != nil {
			return fmt.Errorf("saving TUI telemetry receipt: %w", err)
		}
	case "queued":
		if screen != "" {
			r.screens[screen] = true
		}
	default:
		r.failures++
		return fmt.Errorf("reporting TUI telemetry: unexpected receipt %q", receipt.Status)
	}

	return nil
}

// A report that doesn't finish within 50 ms may be lost.
func (r *tuiTelemetry) sessionEnded(elapsed time.Duration) {
	var sends posthog.SendGroup
	defer sends.Wait(50 * time.Millisecond)
	sends.Go(func() {
		_ = r.report(context.Background(), &apiclient.ReportTelemetryEventBody{
			Event:      telemetry.EventSessionEnded,
			Properties: &apiclient.TelemetryEventProperties{Surface: "tui", DurationBucket: telemetry.DurationBucket(elapsed)},
		})
	})
}
