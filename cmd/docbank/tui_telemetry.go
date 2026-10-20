package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"

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
	path := filepath.Join(layout.Root, "telemetry-tui-state")
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
	lines := strings.Split(string(data), "\n")
	cached := make(map[string]bool)
	valid := lines[0] == day
	for _, name := range lines[1:] {
		valid = valid && slices.Contains(telemetry.ScreenNames(), name)
		cached[name] = true
	}
	if valid {
		r.disabled, r.screens = len(lines) == 1, cached
		if r.disabled || r.screens[screen] {
			return nil
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
		r.failures++
		return fmt.Errorf("reporting TUI telemetry: %w", err)
	}
	switch receipt.Status {
	case "disabled":
		r.disabled = true
	case "queued":
		if screen != "" {
			r.screens[screen] = true
		}
	default:
		r.failures++
		return fmt.Errorf("reporting TUI telemetry: unexpected receipt %q", receipt.Status)
	}

	if r.disabled || screen != "" {
		state := day
		if !r.disabled {
			state += "\n" + strings.Join(slices.Sorted(maps.Keys(r.screens)), "\n")
		}
		return atomicfile.WriteFile(path, []byte(state), atomicfile.WithPrivate())
	}
	return nil
}
