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
)

// tuiTelemetry remembers daemon opt-out receipts and bounds reports within a TUI run.
type tuiTelemetry struct {
	tuiTelemetryReceipt

	mu       sync.Mutex
	failures int
}

type tuiTelemetryReceipt struct {
	Version  int      `json:"version"`
	Day      string   `json:"day"`
	Disabled bool     `json:"disabled"`
	Screens  []string `json:"screens"`
}

func (r *tuiTelemetry) report(ctx context.Context, body *apiclient.ReportTelemetryEventBody) (err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	day := time.Now().UTC().Format(time.DateOnly)
	if r.Day != day {
		r.tuiTelemetryReceipt, r.failures = tuiTelemetryReceipt{Version: 1, Day: day}, 0
	}
	screen := body.Properties.Screen
	if r.Disabled || slices.Contains(r.Screens, screen) || r.failures >= 6 {
		return nil
	}
	defer func() {
		if err != nil {
			r.failures++
		}
	}()
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
		for key := range fields {
			if !slices.Contains([]string{"version", "day", "disabled", "screens"}, key) {
				return fmt.Errorf("unknown TUI telemetry receipt field %q", key)
			}
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
