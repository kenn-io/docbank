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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestTUITelemetryReceiptsSurviveRestart(t *testing.T) {
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
			}))
			defer server.Close()
			var discovery atomic.Int32
			running := func(context.Context) (*daemonconn.Connection, bool, error) {
				discovery.Add(1)
				return daemonconn.New(server.URL, "synthetic-key"), true, nil
			}
			backends := []*tuiDaemonBackend{{telemetry: tuiTelemetry{running: running}}, {telemetry: tuiTelemetry{running: running}}}
			var group sync.WaitGroup
			for _, backend := range backends {
				group.Go(func() { assert.NoError(t, backend.ReportScreen(t.Context(), "help")) })
			}
			group.Wait()
			require.Equal(t, int32(1), requests.Load())
			require.Equal(t, int32(1), discovery.Load())
			info, err := os.Stat(filepath.Join(root, "telemetry-tui.json"))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			if receipt == "disabled" {
				require.NoError(t, os.Remove(filepath.Join(root, "telemetry-tui.json")))
				for _, backend := range backends {
					require.NoError(t, backend.ReportScreen(t.Context(), "browse"))
					backend.telemetry.sessionEnded(1)
				}
				assert.NoFileExists(t, filepath.Join(root, "telemetry-tui.json"))
				assert.Equal(t, int32(1), discovery.Load())
			}
		})
	}
}

func TestTUITelemetryFailureCapSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	synctest.Test(t, func(t *testing.T) {
		var discovery atomic.Int32
		running := func(context.Context) (*daemonconn.Connection, bool, error) {
			discovery.Add(1)
			return nil, false, nil
		}
		for attempt := range 6 {
			for range 2 {
				backend := &tuiDaemonBackend{telemetry: tuiTelemetry{running: running}}
				require.NoError(t, backend.ReportScreen(t.Context(), "help"))
			}
			assert.Equal(t, int32(attempt+1), discovery.Load())
			time.Sleep(time.Second * time.Duration(1<<attempt))
		}
		backend := &tuiDaemonBackend{telemetry: tuiTelemetry{running: running}}
		require.NoError(t, backend.ReportScreen(t.Context(), "help"))
		assert.Equal(t, int32(6), discovery.Load())
	})
}

func TestTUITelemetryDoesNotEnsureDaemon(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	backend := &tuiDaemonBackend{ensure: func(context.Context) (*daemonconn.Connection, error) {
		t.Fatal("telemetry must not start a daemon")
		return nil, context.Canceled
	}}
	require.NoError(t, backend.ReportScreen(t.Context(), "help"))
	_, running, err := daemonconn.Running(t.Context())
	require.NoError(t, err)
	assert.False(t, running)
}
