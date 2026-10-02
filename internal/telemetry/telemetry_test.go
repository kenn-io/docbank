package telemetry

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/docbank/internal/jobs"
)

// enableTelemetryEnv pins both opt-out variables on so a developer's environment can't flip the branch.
func enableTelemetryEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnabledEnv, "1")
	t.Setenv(kittelemetry.GenericTelemetryEnabledEnv, "1")
}

func postEvent(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/daemon/telemetry/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestNewOptedOut(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{"docbank variable", EnabledEnv, "0"},
		{"generic variable", kittelemetry.GenericTelemetryEnabledEnv, "0"},
		{"docbank variable with spaces", EnabledEnv, " 0 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableTelemetryEnv(t)
			t.Setenv(tc.env, tc.value)
			path := filepath.Join(t.TempDir(), "telemetry.json")
			reporter := New(Options{InstallPath: path, Logger: discardLogger()})
			assert.False(t, reporter.Enabled())
			for _, event := range []string{EventAppOpened, EventDaemonStarted, EventDaemonActive} {
				assert.True(t, reporter.EventAllowed(event), event)
			}
			assert.False(t, reporter.EventAllowed("search_run"))
			handler := CaptureHandler(reporter)
			rec := postEvent(t, handler, `{"event":"app_opened"}`)
			assert.Equal(t, http.StatusAccepted, rec.Code)
			assert.JSONEq(t, `{"status":"disabled"}`, rec.Body.String())
			assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, `{"event":"search_run"}`).Code)
			assert.NoFileExists(t, path)
		})
	}
}

func TestNewUnderGoTestAdmitsNothing(t *testing.T) {
	enableTelemetryEnv(t)
	path := filepath.Join(t.TempDir(), "telemetry.json")
	reporter := New(Options{InstallPath: path, Logger: discardLogger()})
	assert.False(t, reporter.Enabled())
	assert.False(t, reporter.EventAllowed(EventAppOpened))
	assert.Equal(t, http.StatusBadRequest, postEvent(t, CaptureHandler(reporter), `{"event":"app_opened"}`).Code)
	assert.NoFileExists(t, path)
}

type postHogBatch struct {
	APIKey string `json:"api_key"`
	Batch  []struct {
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

	path := filepath.Join(t.TempDir(), "telemetry.json")
	r := newEnabled(Options{InstallPath: path, Version: "test-version", Commit: "test-commit", Logger: discardLogger(), endpoint: srv.URL})
	require.True(t, r.Enabled())
	handler := CaptureHandler(r)
	rec := postEvent(t, handler, `{"event":"app_opened","properties":{"path":"/synthetic/report.pdf","query":"synthetic"}}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.JSONEq(t, `{"status":"queued"}`, rec.Body.String())
	assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, `{"event":"search_run"}`).Code)
	require.NoError(t, r.Capture(EventDaemonActive, nil))
	require.NoError(t, r.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var inst install
	require.NoError(t, json.Unmarshal(data, &inst))

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
			require.Contains(t, props, "install_age_hours")
			assert.Zero(t, props["install_age_hours"])
			assert.Equal(t, false, props["$process_person_profile"])
			assert.Equal(t, true, props["$geoip_disable"])
			assert.NotContains(t, props, "path")
			assert.NotContains(t, props, "query")
		}
	}
	assert.ElementsMatch(t, []string{EventAppOpened, EventDaemonActive}, events)
}

func TestInstallIDStableAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.json")
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	first, err := loadOrCreateInstall(path, now)
	require.NoError(t, err)
	second, err := loadOrCreateInstall(path, now.Add(time.Hour))
	require.NoError(t, err)
	assert.NotEmpty(t, first.ID)
	assert.Equal(t, first.ID, second.ID)
	assert.True(t, first.InstalledAt.Equal(second.InstalledAt))
	assert.True(t, now.Equal(second.InstalledAt))
	requirePrivateInstallFile(t, path)
}

func TestNewEnabledFallsBackWhenInstallUnusable(t *testing.T) {
	enableTelemetryEnv(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("synthetic"), 0o600))
	cases := []struct {
		name, path string
		contents   []byte
	}{
		{name: "parent is a file", path: filepath.Join(blocker, "telemetry.json")},
		{name: "empty install id", path: filepath.Join(dir, "empty.json"), contents: []byte(`{"install_id":""}`)},
		{name: "not json", path: filepath.Join(dir, "corrupt.json"), contents: []byte("not json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.contents != nil {
				require.NoError(t, os.WriteFile(tc.path, tc.contents, 0o600))
			}
			var reporter *Reporter
			require.NotPanics(t, func() { reporter = newEnabled(Options{InstallPath: tc.path, Logger: discardLogger()}) })
			assert.False(t, reporter.Enabled())
			assert.False(t, reporter.EventAllowed(EventAppOpened))
			if tc.contents != nil {
				after, err := os.ReadFile(tc.path)
				require.NoError(t, err)
				assert.Equal(t, tc.contents, after)
			}
		})
	}
}

func TestHeartbeatCapturesAtStartAndEachTick(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	captured := make(chan string, 16)
	var calls atomic.Int32
	capture := func(event string) error {
		captured <- event
		if calls.Add(1) == 2 {
			return errors.New("synthetic capture failure")
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHeartbeat(ctx, capture, ticks, discardLogger())
	}()
	assert.Equal(t, EventDaemonStarted, <-captured)
	assert.Equal(t, EventDaemonActive, <-captured)
	ticks <- time.Now()
	assert.Equal(t, EventDaemonActive, <-captured)
	ticks <- time.Now()
	assert.Equal(t, EventDaemonActive, <-captured)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not return after cancel")
	}
	assert.Empty(t, captured)
	assert.Equal(t, int32(4), calls.Load())
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestCloseWithinReturnsAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		called := make(chan struct{})
		closer := closerFunc(func() error {
			close(called)
			<-release
			return nil
		})
		start := time.Now()
		CloseWithin(closer, 50*time.Millisecond, discardLogger())
		assert.Equal(t, 50*time.Millisecond, time.Since(start))
		<-called
		close(release)
	})
}

// TestHeartbeatDrainsBeforeClose checks that the supervisor drain returns only
// after the heartbeat job exits, so a reporter close deferred before the
// supervisor (runServe's order) starts after the heartbeat has stopped.
func TestHeartbeatDrainsBeforeClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger := discardLogger()
		captureRelease := make(chan struct{})
		heartbeatInCapture := make(chan struct{})
		var heartbeatExited, closeSawHeartbeatExited atomic.Bool
		closeStarted := make(chan struct{})
		closer := closerFunc(func() error {
			closeSawHeartbeatExited.Store(heartbeatExited.Load())
			close(closeStarted)
			return nil
		})
		var drainErr error
		func() {
			defer CloseWithin(closer, time.Second, logger)
			supervisor := jobs.New(t.Context(), logger)
			defer func() {
				drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				drainErr = supervisor.Shutdown(drainCtx)
			}()
			var once sync.Once
			require.NoError(t, supervisor.Start(HeartbeatJobName, func(ctx context.Context) error {
				defer heartbeatExited.Store(true)
				runHeartbeat(ctx, func(string) error {
					once.Do(func() { close(heartbeatInCapture) })
					<-captureRelease
					return nil
				}, nil, logger)
				return nil
			}))
			<-heartbeatInCapture
			time.AfterFunc(50*time.Millisecond, func() { close(captureRelease) })
		}()
		require.NoError(t, drainErr)
		<-closeStarted
		assert.True(t, closeSawHeartbeatExited.Load(), "reporter close started before the heartbeat returned")
	})
}
