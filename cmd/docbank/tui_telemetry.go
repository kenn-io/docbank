package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	doctui "go.kenn.io/docbank/internal/tui"
)

// tuiTelemetry remembers daemon opt-out receipts and bounds reports within a TUI run.
type tuiTelemetry struct {
	tuiTelemetryReceipt

	mu       sync.Mutex
	failures int
	retryAt  time.Time
}

type tuiTelemetryReceipt struct {
	Version  int      `json:"version"`
	Day      string   `json:"day"`
	Disabled bool     `json:"disabled"`
	Screens  []string `json:"screens"`
}

var errTUITelemetryBackoff = errors.New("reporting TUI telemetry: waiting after a failed report")

func (r *tuiTelemetry) report(ctx context.Context, body *apiclient.ReportTelemetryEventBody) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	day := now.UTC().Format(time.DateOnly)
	r.resetDay(day)
	screen := body.Properties.Screen
	if r.Disabled || slices.Contains(r.Screens, screen) || r.failures >= 6 {
		return nil
	}
	if now.Before(r.retryAt) {
		return &doctui.ScreenRetryError{Err: errTUITelemetryBackoff, At: r.retryAt}
	}
	// The attempt timeout starts after the gate, so queued reports keep their full second.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	defer func() { err = r.failed(err) }()
	layout, err := home.Resolve()
	if err != nil {
		return fmt.Errorf("resolving telemetry state: %w", err)
	}
	path := filepath.Join(layout.Root, "telemetry-tui-state.json")
	lock := flock.New(path + ".lock")
	_, err = lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("locking TUI telemetry: %w", err)
	}
	defer func() { _ = lock.Close() }()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading TUI telemetry receipt: %w", err)
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) == nil {
		if string(fields["version"]) != "1" {
			return errors.New("unsupported TUI telemetry receipt version")
		}
		var state tuiTelemetryReceipt
		if json.Unmarshal(data, &state) == nil && state.Day == day {
			r.tuiTelemetryReceipt = state
			if r.Disabled || slices.Contains(r.Screens, screen) {
				return nil
			}
		}
	}
	c, ok, err := daemonconn.Running(ctx)
	if err != nil {
		return fmt.Errorf("finding daemon for telemetry: %w", err)
	}
	if !ok {
		return errors.New("reporting TUI telemetry: daemon is not running")
	}
	defer func() { _ = c.Close() }()
	receipt, err := c.API().ReportTelemetryEvent(ctx, &apiclient.ReportTelemetryEventRequestOptions{Body: body})
	if err != nil {
		return fmt.Errorf("reporting TUI telemetry: %w", err)
	}
	switch receipt.Status {
	case "disabled":
		r.Disabled = true
	case "queued":
		if screen != "" {
			r.Screens = append(r.Screens, screen)
		}
	default:
		return fmt.Errorf("reporting TUI telemetry: unexpected receipt %q", receipt.Status)
	}

	if r.Disabled || screen != "" {
		data, err := json.Marshal(r.tuiTelemetryReceipt)
		if err != nil {
			return err
		}
		return atomicfile.WriteFile(path, data, atomicfile.WithPrivate())
	}
	return nil
}

func (r *tuiTelemetry) resetDay(day string) {
	if r.Day != day {
		r.tuiTelemetryReceipt = tuiTelemetryReceipt{Version: 1, Day: day}
		r.failures = 0
		r.retryAt = time.Time{}
	}
}

// failed counts a failed attempt and starts the window before the next attempt may begin.
func (r *tuiTelemetry) failed(err error) error {
	if err == nil {
		return nil
	}
	r.failures++
	r.retryAt = time.Now().Add(doctui.ScreenRetryDelay)
	return &doctui.ScreenRetryError{Err: err, At: r.retryAt}
}
