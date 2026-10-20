package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
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
	doctui "go.kenn.io/docbank/internal/tui"
)

func setupTUITelemetryDaemon(t *testing.T, receipt string) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	setupFakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
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
	t.Run("unsupported version", func(t *testing.T) {
		requests := setupTUITelemetryDaemon(t, "queued")
		path := filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json")
		value := `{"version":2,"disabled":true}`
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
		backend := &tuiDaemonBackend{}
		require.Error(t, backend.ReportScreen(t.Context(), "help"))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, value, string(data))
		assert.Zero(t, requests.Load())
	})
	for _, tc := range []struct {
		name    string
		value   string
		screens []string
	}{
		{"stale day", `{"version":1,"day":"2000-01-01","disabled":true}`, []string{"help"}},
		{"opaque screen", `{"version":1,"day":"` + day + `","screens":["future_screen"]}`, []string{"future_screen", "help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, "queued")
			path := filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.value), 0o600))
			backend := &tuiDaemonBackend{}
			require.NoError(t, backend.ReportScreen(t.Context(), "help"))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var receipt tuiTelemetryReceipt
			require.NoError(t, json.Unmarshal(data, &receipt))
			assert.Equal(t, tc.screens, receipt.Screens)
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestTUITelemetryBoundsEveryFailure(t *testing.T) {
	setupTUITelemetryDaemon(t, "future")
	backend := &tuiDaemonBackend{}
	for range 6 {
		backend.telemetry.retryAt = time.Time{} // Each failure follows an elapsed retry window.
		require.ErrorContains(t, backend.ReportScreen(t.Context(), "help"), `unexpected receipt "future"`)
	}
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, backend.ReportScreen(t.Context(), "browse"))
}

func TestTUITelemetryQueuedScreensRespectFailureBackoff(t *testing.T) {
	firstPOST := make(chan struct{})
	release := make(chan struct{}, 1)
	var requests atomic.Int32
	setupFakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(firstPOST)
			<-release
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	t.Cleanup(func() { close(release) })
	backend := &tuiDaemonBackend{}
	firstResult := make(chan error, 1)
	go func() { firstResult <- backend.ReportScreen(t.Context(), "help") }()
	<-firstPOST
	results := make(chan error, 3)
	for _, screen := range []string{"browse", "search", "help"} {
		go func() {
			results <- backend.ReportScreen(t.Context(), screen)
		}()
	}
	exitResult := make(chan error, 1)
	go func() {
		exitResult <- sendTUISessionEnded(t.Context(), &backend.telemetry, time.Minute)
	}()
	// All callers must be waiting on the report mutex before the first failure sets backoff.
	require.Eventually(t, func() bool {
		var stacks bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&stacks, 2); err != nil {
			return false
		}
		queued := 0
		for stack := range strings.SplitSeq(stacks.String(), "\n\n") {
			if strings.Contains(stack, "[sync.Mutex.Lock]") && strings.Contains(stack, ".(*tuiTelemetry).report(") {
				queued++
			}
		}
		return queued == 4
	}, time.Second, time.Millisecond)
	release <- struct{}{}
	require.Error(t, <-firstResult)
	for range 3 {
		require.ErrorIs(t, <-results, errTUITelemetryBackoff)
	}
	require.ErrorIs(t, <-exitResult, errTUITelemetryBackoff)
	assert.Equal(t, int32(1), requests.Load(), "queued screen and exit reports must wait for the 30-second backoff")
	assert.Equal(t, 1, backend.telemetry.failures, "queued screen and exit reports must preserve the remaining failure budget")
}

func TestTUITelemetryNewUTCDayResetsBackoff(t *testing.T) {
	requests := setupTUITelemetryDaemon(t, "queued")
	now := time.Now()
	backend := &tuiDaemonBackend{}
	backend.telemetry.tuiTelemetryReceipt = tuiTelemetryReceipt{
		Version: 1,
		Day:     now.UTC().AddDate(0, 0, -1).Format(time.DateOnly),
		Screens: []string{"help", "browse"},
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
	for _, failure := range []string{"busy", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, "queued")
			lock := flock.New(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.json.lock"))
			require.NoError(t, lock.Lock())
			defer func() { require.NoError(t, lock.Close()) }()
			backend := &tuiDaemonBackend{}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
				defer cancel()
				wantErr := context.DeadlineExceeded
				if failure == "cancelled" {
					cancel()
					wantErr = context.Canceled
				}
				err := backend.ReportScreen(ctx, "help")
				require.ErrorIs(t, err, wantErr)
				var retryErr *doctui.ScreenRetryError
				require.ErrorAs(t, err, &retryErr)
				assert.Equal(t, time.Now().Add(doctui.ScreenRetryDelay), retryErr.At)
			})
			assert.Zero(t, requests.Load())
		})
	}
}
