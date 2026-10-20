package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func setupTUITelemetryDaemon(t *testing.T, status int, receipt string) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	setupFakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
	})
	return &requests
}

func TestTUITelemetryReceipts(t *testing.T) {
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			var requests atomic.Int32
			setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/daemon/telemetry/events", r.URL.Path)
				assert.Equal(t, "POST", r.Method)
				assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
				var request api.TelemetryEventRequest
				assert.NoError(t, json.UnmarshalRead(r.Body, &request))
				want := api.TelemetryEventRequest{Event: "screen_viewed", Properties: &api.TelemetryEventProperties{Screen: "help", Surface: "tui"}}
				if requests.Add(1) == 2 {
					want = api.TelemetryEventRequest{Event: "session_ended", Properties: &api.TelemetryEventProperties{Surface: "tui", DurationBucket: "under_1m"}}
				}
				assert.Equal(t, want, request)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
			})
			root := os.Getenv("DOCBANK_HOME")
			var group sync.WaitGroup
			for range 2 {
				group.Go(func() {
					backend := &tuiDaemonBackend{}
					assert.NoError(t, backend.ReportScreen(t.Context(), "help"))
				})
			}
			group.Wait()
			assert.Equal(t, int32(1), requests.Load())
			require.NoError(t, sendTUISessionEnded(t.Context(), &tuiTelemetry{}, time.Second))
			if receipt == "queued" {
				assert.Equal(t, int32(2), requests.Load())
			} else {
				assert.Equal(t, int32(1), requests.Load())
			}
			runtimePath, err := daemonconn.RuntimeStore(root).Path(os.Getpid())
			require.NoError(t, err)
			require.NoError(t, os.Remove(runtimePath))
			restarted := &tuiDaemonBackend{}
			require.NoError(t, restarted.ReportScreen(t.Context(), "help"))
			path := filepath.Join(root, "telemetry-tui-state.json")
			require.NoError(t, os.Remove(path))
			require.NoError(t, restarted.ReportScreen(t.Context(), "help"))
			if receipt == "disabled" {
				require.NoError(t, restarted.ReportScreen(t.Context(), "browse"))
				require.NoError(t, sendTUISessionEnded(t.Context(), &restarted.telemetry, time.Minute))
			}
			assert.NoFileExists(t, path)
		})
	}
}

func TestTUITelemetryStoredStates(t *testing.T) {
	day := time.Now().UTC().Format(time.DateOnly)
	for _, tc := range []struct {
		name    string
		value   string
		refused bool
	}{
		{"stale day", `{"version":1,"day":"2000-01-01","disabled":true}`, false},
		{"unsupported version", `{"version":2,"disabled":true}`, true},
		{"opaque screen", `{"version":1,"day":"` + day + `","screens":["future_screen"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
			path := filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.value), 0o600))
			backend := &tuiDaemonBackend{}
			err := backend.ReportScreen(t.Context(), "help")
			data, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			if tc.refused {
				require.Error(t, err)
				assert.Equal(t, tc.value, string(data))
				assert.Zero(t, requests.Load())
			} else {
				require.NoError(t, err)
				assert.Contains(t, string(data), `"help"`)
				if strings.Contains(tc.value, "future_screen") {
					assert.Contains(t, string(data), `"future_screen"`)
				}
				assert.Equal(t, int32(1), requests.Load())
			}
		})
	}
}

func TestTUITelemetryBoundsEveryFailure(t *testing.T) {
	for _, failure := range []string{"lock", "post"} {
		t.Run(failure, func(t *testing.T) {
			status := http.StatusAccepted
			if failure == "post" {
				status = http.StatusServiceUnavailable
			}
			setupTUITelemetryDaemon(t, status, "future")
			backend := &tuiDaemonBackend{}
			for range 6 {
				backend.telemetry.retryAt = time.Time{} // Each failure follows an elapsed retry window.
				ctx, cancel := context.WithCancel(t.Context())
				if failure == "lock" {
					cancel()
				}
				require.Error(t, backend.ReportScreen(ctx, "help"))
				cancel()
			}
			t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "missing"))
			require.NoError(t, backend.ReportScreen(t.Context(), "browse"))
		})
	}
}

func TestTUITelemetryQueuedScreensRespectFailureBackoff(t *testing.T) {
	firstPOST := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	setupFakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(firstPOST)
			<-release
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	backend := &tuiDaemonBackend{}
	firstResult := make(chan error, 1)
	go func() { firstResult <- backend.ReportScreen(t.Context(), "help") }()
	<-firstPOST
	var started sync.WaitGroup
	started.Add(4)
	results := make(chan error, 3)
	for _, screen := range []string{"browse", "search", "help"} {
		go func() {
			started.Done()
			results <- backend.ReportScreen(t.Context(), screen)
		}()
	}
	exitResult := make(chan error, 1)
	go func() {
		started.Done()
		exitResult <- sendTUISessionEnded(t.Context(), &backend.telemetry, time.Minute)
	}()
	started.Wait()
	failedAt := time.Now()
	close(release)
	require.Error(t, <-firstResult)
	for range 3 {
		require.ErrorIs(t, <-results, errTUITelemetryBackoff)
	}
	require.ErrorIs(t, <-exitResult, errTUITelemetryBackoff)
	assert.Less(t, time.Since(failedAt), 30*time.Second)
	assert.Equal(t, int32(1), requests.Load(), "queued screen and exit reports must wait for the 30-second backoff")
	assert.Equal(t, 1, backend.telemetry.failures, "queued screen and exit reports must preserve the remaining failure budget")
}

func TestTUITelemetryNewUTCDayResetsBackoff(t *testing.T) {
	requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
	now := time.Now()
	backend := &tuiDaemonBackend{}
	backend.telemetry.tuiTelemetryReceipt = tuiTelemetryReceipt{
		Version:  1,
		Day:      now.UTC().AddDate(0, 0, -1).Format(time.DateOnly),
		Disabled: true,
		Screens:  []string{"help", "browse"},
	}
	backend.telemetry.failures = 6
	backend.telemetry.retryAt = now.Add(time.Hour)

	require.NoError(t, backend.ReportScreen(t.Context(), "help"))
	assert.Equal(t, int32(1), requests.Load(), "a new UTC day must permit a fresh attempt")
	data, err := os.ReadFile(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json"))
	require.NoError(t, err)
	var receipt tuiTelemetryReceipt
	require.NoError(t, json.Unmarshal(data, &receipt))
	assert.Equal(t, tuiTelemetryReceipt{
		Version: 1,
		Day:     now.UTC().Format(time.DateOnly),
		Screens: []string{"help"},
	}, receipt)
}

func TestTUITelemetryBusyLockReturnsRetryError(t *testing.T) {
	requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
	lock := flock.New(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json.lock"))
	require.NoError(t, lock.Lock())
	defer func() { require.NoError(t, lock.Close()) }()
	backend := &tuiDaemonBackend{}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, backend.ReportScreen(ctx, "help"), context.DeadlineExceeded)
	})
	assert.Zero(t, requests.Load())
	require.NoError(t, lock.Unlock())
	require.NoError(t, backend.ReportScreen(t.Context(), "help"))
	assert.Equal(t, int32(1), requests.Load())
}
