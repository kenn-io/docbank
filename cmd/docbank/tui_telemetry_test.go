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

func setupTUITelemetryDaemon(t *testing.T, status int, receipt string, visits ...api.TelemetryEventRequest) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/daemon/telemetry/events", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		var request api.TelemetryEventRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		want := api.TelemetryEventRequest{Event: "screen_viewed", Properties: &api.TelemetryEventProperties{Screen: "help", Surface: "tui"}}
		if requests.Add(1) > 1 && len(visits) > 0 {
			want = visits[0]
		}
		assert.Equal(t, want, request)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
	})
	return &requests
}

func TestTUITelemetryReceipts(t *testing.T) {
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, http.StatusAccepted, receipt, api.TelemetryEventRequest{
				Event: "session_ended", Properties: &api.TelemetryEventProperties{Surface: "tui", DurationBucket: "under_1m"},
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
			path := filepath.Join(root, "telemetry-tui-state")
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
		{"future day", `{"version":1,"day":"2999-01-01","disabled":true}`, false},
		{"unsupported version", `{"version":2,"disabled":true}`, true},
		{"unknown field", `{"version":1,"future":true}`, true},
		{"opaque screen", `{"version":1,"day":"` + day + `","screens":["future_screen"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
			path := filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state")
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
	for _, failure := range []string{"file", "lock", "discovery", "post", "receipt"} {
		t.Run(failure, func(t *testing.T) {
			status := http.StatusAccepted
			if failure == "post" {
				status = http.StatusServiceUnavailable
			}
			setupTUITelemetryDaemon(t, status, "future")
			if failure == "discovery" {
				t.Setenv("DOCBANK_HOME", t.TempDir())
			}
			if failure == "file" {
				require.NoError(t, os.Mkdir(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state"), 0o700))
			}
			backend := &tuiDaemonBackend{}
			for range 6 {
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

func TestTUITelemetryBusyLockReturnsRetryError(t *testing.T) {
	requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
	lock := flock.New(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-state.lock"))
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
