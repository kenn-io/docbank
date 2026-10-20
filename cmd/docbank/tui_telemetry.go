package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

type tuiTelemetryClaim struct {
	Accepted bool      `json:"accepted,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	Next     time.Time `json:"next,omitzero"`
}

type tuiTelemetryState struct {
	Day      string                       `json:"day"`
	Disabled bool                         `json:"disabled,omitempty"`
	Screens  map[string]tuiTelemetryClaim `json:"screens"`
}

// tuiTelemetry keeps daemon receipts authoritative across TUI runs.
type tuiTelemetry struct {
	mu      sync.Mutex
	state   tuiTelemetryState
	running func(context.Context) (*daemonconn.Connection, bool, error)
}

func (r *tuiTelemetry) report(ctx context.Context, body *apiclient.ReportTelemetryEventBody) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if body.Properties.Screen != "" && !slices.Contains(telemetry.ScreenNames(), body.Properties.Screen) {
		return nil
	}
	day := time.Now().UTC().Format(time.DateOnly)
	if r.state.Day == day && r.state.Disabled {
		return nil
	}
	layout, err := home.Resolve()
	if err != nil {
		return fmt.Errorf("resolving telemetry state: %w", err)
	}
	path := filepath.Join(layout.Root, "telemetry-tui.json")
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("locking TUI telemetry: %w", err)
	}
	if !locked {
		return nil
	}
	defer func() { _ = lock.Close() }()
	state := tuiTelemetryState{Day: day, Screens: make(map[string]tuiTelemetryClaim)}
	data, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &state) != nil {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading TUI telemetry: %w", err)
	}
	if state.Day != day {
		state = tuiTelemetryState{Day: day, Screens: make(map[string]tuiTelemetryClaim)}
	}
	r.state = state
	if state.Disabled {
		return nil
	}
	screen := body.Properties.Screen
	claim := state.Screens[screen]
	if claim.Accepted || claim.Attempts >= 6 || time.Now().Before(claim.Next) {
		return nil
	}
	running := r.running
	if running == nil {
		running = daemonconn.Running
	}
	c, ok, reportErr := running(ctx)
	var receipt *apiclient.ReportTelemetryEventResponse
	if reportErr == nil && ok {
		receipt, reportErr = c.API().ReportTelemetryEvent(ctx, &apiclient.ReportTelemetryEventRequestOptions{Body: body})
		_ = c.Close()
	}
	claim.Attempts++
	claim.Next = time.Now().Add(time.Second * time.Duration(1<<(claim.Attempts-1)))
	if receipt != nil && reportErr == nil {
		state.Disabled = receipt.Status == "disabled"
		if receipt.Status == "queued" {
			claim = tuiTelemetryClaim{Accepted: screen != ""}
		}
	}
	if state.Screens == nil {
		state.Screens = make(map[string]tuiTelemetryClaim)
	}
	state.Screens[screen] = claim
	r.state = state
	data, err = json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding TUI telemetry: %w", err)
	}
	if err := atomicfile.WriteFile(path, data); err != nil {
		return fmt.Errorf("saving TUI telemetry: %w", err)
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
