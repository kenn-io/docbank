package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestTUITelemetryReceipts(t *testing.T) {
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			var requests, discovery atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
			}))
			defer server.Close()
			running := func(context.Context) (*daemonconn.Connection, bool, error) {
				discovery.Add(1)
				return daemonconn.New(server.URL, "synthetic-key"), true, nil
			}
			backends := []*tuiDaemonBackend{{telemetry: tuiTelemetry{running: running}}, {telemetry: tuiTelemetry{running: running}}}
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
			restarted := &tuiDaemonBackend{telemetry: tuiTelemetry{running: running}}
			require.NoError(t, restarted.ReportScreen(t.Context(), "browse"))
			require.NoError(t, os.Remove(path))
			for _, backend := range append(backends, restarted) {
				require.NoError(t, backend.ReportScreen(t.Context(), "browse"))
				backend.telemetry.sessionEnded(time.Minute)
			}
			assert.NoFileExists(t, path)
			assert.Equal(t, int32(1), requests.Load())
			assert.Equal(t, int32(1), discovery.Load())
		})
	}
}

func TestTUITelemetryUnexpectedDateIsEmpty(t *testing.T) {
	for _, value := range []string{"", "invalid", "2000-01-01", time.Now().UTC().Format(time.DateOnly) + "\n"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			require.NoError(t, os.WriteFile(filepath.Join(root, "telemetry-tui-disabled"), []byte(value), 0o600))
			var discovery int
			backend := &tuiDaemonBackend{telemetry: tuiTelemetry{running: func(context.Context) (*daemonconn.Connection, bool, error) {
				discovery++
				return nil, false, nil
			}}}
			require.ErrorContains(t, backend.ReportScreen(t.Context(), "help"), "daemon is not running")
			assert.Equal(t, 1, discovery)
		})
	}
}

func TestTUITelemetryPostFailureCap(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	backend := &tuiDaemonBackend{telemetry: tuiTelemetry{running: func(context.Context) (*daemonconn.Connection, bool, error) {
		return daemonconn.New(server.URL, "synthetic-key"), true, nil
	}}}
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
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer server.Close()
	backend.telemetry.running = func(context.Context) (*daemonconn.Connection, bool, error) {
		return daemonconn.New(server.URL, "synthetic-key"), true, nil
	}
	require.NoError(t, backend.ReportScreen(t.Context(), "help"))
	assert.Equal(t, int32(1), requests.Load())
}

func TestTUITelemetryBusyLockReturnsRetryError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	synctest.Test(t, func(t *testing.T) {
		lock := flock.New(filepath.Join(root, "telemetry-tui-disabled.lock"))
		require.NoError(t, lock.Lock())
		defer func() { require.NoError(t, lock.Close()) }()
		var discovery int
		backend := &tuiDaemonBackend{telemetry: tuiTelemetry{running: func(context.Context) (*daemonconn.Connection, bool, error) {
			discovery++
			return nil, false, nil
		}}}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, backend.ReportScreen(ctx, "help"), context.DeadlineExceeded)
		assert.Zero(t, discovery)
		require.NoError(t, lock.Unlock())
		require.ErrorContains(t, backend.ReportScreen(t.Context(), "help"), "daemon is not running")
		assert.Equal(t, 1, discovery)
	})
}

func TestTUITelemetryAfterDaemonIdleExit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("[server]\nidle_timeout = \"100ms\"\n"), 0o600))
	t.Setenv("DOCBANK_HOME", root)
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "0")
	t.Setenv(daemonconn.EnvBackgroundDaemon, "1")
	startTestDaemon(t, root)
	initial, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	require.NoError(t, initial.Close())
	require.Eventually(t, func() bool {
		c, running, err := daemonconn.Running(t.Context())
		if c != nil {
			_ = c.Close()
		}
		return err == nil && !running
	}, 5*time.Second, 200*time.Millisecond)
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		t.Fatal("opening Help after idle exit must not start a daemon")
		return nil, context.Canceled
	}}
	require.ErrorContains(t, backend.ReportScreen(t.Context(), "help"), "daemon is not running")
	_, running, err := daemonconn.Running(t.Context())
	require.NoError(t, err)
	assert.False(t, running)
}
