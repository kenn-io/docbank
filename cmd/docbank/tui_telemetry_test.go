package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func setupTUITelemetryDaemon(t *testing.T, status int, receipt string) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/daemon/telemetry/events", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
	})
	return &requests
}

func TestTUITelemetryReceipts(t *testing.T) {
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, http.StatusAccepted, receipt)
			root := os.Getenv("DOCBANK_HOME")
			backends := []*tuiDaemonBackend{{}, {}}
			var group sync.WaitGroup
			for _, backend := range backends {
				group.Go(func() {
					assert.NoError(t, backend.ReportScreen(t.Context(), "help"))
					assert.NoError(t, backend.ReportScreen(t.Context(), "help"))
				})
			}
			group.Wait()
			path := filepath.Join(root, "telemetry-tui-disabled")
			if receipt == "queued" {
				assert.Equal(t, int32(2), requests.Load())
				assert.NoFileExists(t, path)
				return
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, time.Now().UTC().Format(time.DateOnly), string(data))
			runtimePath, err := daemonconn.RuntimeStore(root).Path(os.Getpid())
			require.NoError(t, err)
			require.NoError(t, os.Remove(runtimePath))
			restarted := &tuiDaemonBackend{}
			require.NoError(t, restarted.ReportScreen(t.Context(), "browse"))
			require.NoError(t, os.Remove(path))
			for _, backend := range append(backends, restarted) {
				require.NoError(t, backend.ReportScreen(t.Context(), "browse"))
				backend.telemetry.sessionEnded(time.Minute)
			}
			assert.NoFileExists(t, path)
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestTUITelemetryUnexpectedDateIsEmpty(t *testing.T) {
	for _, value := range []string{"", "invalid", "2000-01-01", time.Now().UTC().Format(time.DateOnly) + "\n"} {
		t.Run(value, func(t *testing.T) {
			requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-disabled"), []byte(value), 0o600))
			backend := &tuiDaemonBackend{}
			require.NoError(t, backend.ReportScreen(t.Context(), "help"))
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestTUITelemetryPostFailureCap(t *testing.T) {
	requests := setupTUITelemetryDaemon(t, http.StatusServiceUnavailable, "")
	backend := &tuiDaemonBackend{}
	for range 6 {
		require.Error(t, backend.ReportScreen(t.Context(), "help"))
	}
	require.NoError(t, backend.ReportScreen(t.Context(), "help"))
	require.NoError(t, backend.ReportScreen(t.Context(), "browse"))
	assert.Equal(t, int32(6), requests.Load())
}

func TestTUITelemetryRetriesMissingDaemonWithoutEnsure(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		t.Fatal("telemetry must not start a daemon")
		return nil, context.Canceled
	}}
	for range 8 {
		require.ErrorContains(t, backend.ReportScreen(t.Context(), "help"), "daemon is not running")
	}
	requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
	require.NoError(t, backend.ReportScreen(t.Context(), "help"))
	assert.Equal(t, int32(1), requests.Load())
}

func TestTUITelemetryBusyLockReturnsRetryError(t *testing.T) {
	requests := setupTUITelemetryDaemon(t, http.StatusAccepted, "queued")
	lock := flock.New(filepath.Join(os.Getenv("DOCBANK_HOME"), "telemetry-tui-disabled.lock"))
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
