package telemetry

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

// enableTelemetryEnv pins both opt-out variables on so a developer's environment can't flip the branch.
func enableTelemetryEnv(t *testing.T) {
	t.Helper()
	if posthog.ProcessDisabled() {
		t.Skip("built with kit_posthog_disabled")
	}
	t.Setenv(EnabledEnv, "1")
	t.Setenv(posthog.GenericEnabledEnv, "1")
}

func postEvent(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/daemon/telemetry/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestNewOptedOut(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{"docbank variable", EnabledEnv, "0"},
		{"generic variable", posthog.GenericEnabledEnv, "0"},
		{"docbank variable spelled off", EnabledEnv, " off "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableTelemetryEnv(t)
			t.Setenv(tc.env, tc.value)
			dir := t.TempDir()
			reporter := New(Options{Dir: dir, Logger: discardLogger()})
			assert.False(t, reporter.Enabled())
			for _, event := range []string{EventAppOpened, EventDaemonStarted, EventDaemonActive} {
				assert.True(t, reporter.EventAllowed(event), event)
			}
			assert.False(t, reporter.EventAllowed("search_run"))
			handler := CaptureHandler(reporter, dir)
			rec := postEvent(t, handler, `{"event":"app_opened"}`)
			assert.Equal(t, http.StatusAccepted, rec.Code)
			assert.JSONEq(t, `{"status":"disabled"}`, rec.Body.String())
			assert.JSONEq(t, `{"status":"disabled"}`, postEvent(t, handler, screenBody("browse", "web")).Body.String())
			assert.NoFileExists(t, filepath.Join(dir, screenClaimsFile))
			assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, screenBody("unknown", "web")).Code)
			assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, `{"event":"search_run"}`).Code)
			assert.NoFileExists(t, filepath.Join(dir, posthog.InstallFileName))
		})
	}
}

type postHogBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		DistinctID string         `json:"distinct_id"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

func TestEnabledReporterSendsOnlyAllowlistedFields(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		if strings.HasPrefix(r.URL.Path, "/batch") {
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	r := New(Options{Dir: dir, Version: "test-version", Commit: "test-commit", Logger: discardLogger(), endpoint: srv.URL})
	require.True(t, r.Enabled())
	handler := CaptureHandler(r, dir)
	rec := postEvent(t, handler, `{"event":"app_opened","properties":{"path":"/synthetic/report.pdf","query":"synthetic"}}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.JSONEq(t, `{"status":"queued"}`, rec.Body.String())
	assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, `{"event":"search_run"}`).Code)
	require.NoError(t, r.Capture(EventDaemonActive, nil))
	require.NoError(t, r.Close())

	inst, err := posthog.LoadOrCreateInstall(dir)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	var events []string
	for _, body := range bodies {
		var batch postHogBatch
		require.NoError(t, json.Unmarshal(body, &batch))
		for _, msg := range batch.Batch {
			events = append(events, msg.Event)
			assert.Equal(t, inst.ID, msg.DistinctID)
			props := msg.Properties
			assert.Equal(t, "docbank", props["application"])
			assert.Equal(t, "daemon", props["source"])
			assert.Equal(t, "test-version", props["version"])
			assert.Equal(t, "test-commit", props["commit"])
			assert.Equal(t, runtime.GOOS, props["goos"])
			assert.Equal(t, runtime.GOARCH, props["goarch"])
			assert.Contains(t, props, "install_age_hours")
			assert.NotContains(t, props, "path")
			assert.NotContains(t, props, "query")
		}
	}
	assert.ElementsMatch(t, []string{EventAppOpened, EventDaemonActive}, events)
}

func TestNewFallsBackWhenInstallUnusable(t *testing.T) {
	enableTelemetryEnv(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("synthetic"), 0o600))
	var reporter *Reporter
	require.NotPanics(t, func() { reporter = New(Options{Dir: blocker, Logger: discardLogger()}) })
	assert.False(t, reporter.Enabled())
	assert.False(t, reporter.EventAllowed(EventAppOpened))
}

func TestReporterUsesDaemonLogger(t *testing.T) {
	enableTelemetryEnv(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("daemon", "synthetic")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic ingest rejection", http.StatusUnauthorized)
	}))
	defer server.Close()
	reporter := New(Options{Dir: t.TempDir(), Logger: logger, endpoint: server.URL})
	require.True(t, reporter.Enabled())
	require.NoError(t, reporter.Capture(EventDaemonActive, nil))
	require.NoError(t, reporter.Close())
	assert.Contains(t, logs.String(), "synthetic ingest rejection")
	assert.Contains(t, logs.String(), `"daemon":"synthetic"`)
	assert.Contains(t, logs.String(), `"component":"posthog"`)
}

type recordingClient struct {
	mu     sync.Mutex
	events []string
}

func (c *recordingClient) Capture(event string, _ map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return nil
}

func (c *recordingClient) Close() error  { return nil }
func (c *recordingClient) Enabled() bool { return true }

func (c *recordingClient) captured() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// Background daemons exit after idle_timeout, so each start must send both events at once.
func TestHeartbeatSendsStartedAndActiveAtStart(t *testing.T) {
	client := &recordingClient{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunHeartbeat(ctx, client, discardLogger())
	}()
	require.Eventually(t, func() bool { return len(client.captured()) == 2 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, []string{EventDaemonStarted, EventDaemonActive}, client.captured())
}
