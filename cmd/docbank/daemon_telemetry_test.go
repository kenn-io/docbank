package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/telemetry"
)

type telemetryTestDaemon struct {
	root    string
	baseURL string
	apiKey  string
}

func startTelemetryTestDaemon(t *testing.T) telemetryTestDaemon {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("DOCBANK_HOME", root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "daemon did not stop")
		}
	})
	var found telemetryTestDaemon
	require.Eventually(t, func() bool {
		rec, _, ok, findErr := daemonconn.Find(ctx, root)
		if findErr != nil || !ok {
			return false
		}
		found = telemetryTestDaemon{root: root, baseURL: "http://" + rec.Address, apiKey: rec.Metadata["api_key"]}
		return true
	}, 30*time.Second, 25*time.Millisecond)
	return found
}

func (d telemetryTestDaemon) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, d.baseURL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", d.apiKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

func (d telemetryTestDaemon) requireNoHeartbeatOrInstallFile(t *testing.T) {
	t.Helper()
	status, jobs := d.do(t, http.MethodGet, "/api/v1/jobs", "")
	require.Equal(t, http.StatusOK, status, jobs)
	assert.NotContains(t, jobs, telemetry.HeartbeatJobName)
	assert.NoFileExists(t, filepath.Join(d.root, posthog.InstallFileName))
}

func TestTUISessionEndedReachesRunningDaemon(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "0")
	startTelemetryTestDaemon(t)
	require.NoError(t, reportTUISessionEnded(2*time.Minute))
}

func TestServeAcceptsAppOpenedWhenTelemetryOptedOut(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "0")
	t.Setenv(posthog.GenericEnabledEnv, "1")
	d := startTelemetryTestDaemon(t)
	status, body := d.do(t, http.MethodPost, "/api/daemon/telemetry/events", `{"event":"app_opened"}`)
	require.Equal(t, http.StatusAccepted, status, body)
	assert.JSONEq(t, `{"status":"disabled"}`, body)
	status, body = d.do(t, http.MethodPost, "/api/daemon/telemetry/events", `{"event":"search_run"}`)
	assert.Equal(t, http.StatusBadRequest, status, body)
	d.requireNoHeartbeatOrInstallFile(t)
}
