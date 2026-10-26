package telemetry

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

// testAPIKey enables reporters that send to a loopback collector.
const testAPIKey = "synthetic-project-key"

// enableTelemetryEnv isolates reporting from inherited opt-outs and CI.
func enableTelemetryEnv(t *testing.T) {
	t.Helper()
	if posthog.ProcessDisabled() {
		t.Skip("built with kit_posthog_disabled")
	}
	t.Setenv("CI", "")
	require.NoError(t, os.Unsetenv("CI"))
	t.Setenv("DO_NOT_TRACK", "0")
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

func TestSessionEndedPropertiesAndBuckets(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	reporter := New(Options{Dir: t.TempDir(), Logger: discardLogger()})
	for _, tc := range []struct {
		elapsed time.Duration
		bucket  string
	}{
		{time.Minute - time.Nanosecond, "under_1m"}, {time.Minute, "1_to_5m"},
		{2 * time.Minute, "1_to_5m"}, {5 * time.Minute, "5_to_30m"},
		{30 * time.Minute, "5_to_30m"}, {30*time.Minute + time.Nanosecond, "over_30m"},
	} {
		assert.Equal(t, tc.bucket, DurationBucket(tc.elapsed))
		props, err := reporter.SanitizeProperties(EventSessionEnded, map[string]any{"surface": "tui", "duration_bucket": tc.bucket, "seconds": 120})
		require.NoError(t, err)
		assert.Equal(t, tc.bucket, props["duration_bucket"])
		assert.Equal(t, "tui", props["surface"])
		assert.NotContains(t, props, "seconds")
	}
	props, err := reporter.SanitizeProperties(EventSessionEnded, map[string]any{"surface": "web"})
	require.NoError(t, err)
	assert.Equal(t, "web", props["surface"])
	for _, value := range []any{nil, "invalid", 120} {
		props, err := reporter.SanitizeProperties(EventSessionEnded, map[string]any{"surface": "tui", "duration_bucket": value})
		require.NoError(t, err)
		assert.Equal(t, "tui", props["surface"])
		assert.NotContains(t, props, "duration_bucket")
	}
}

func TestNewOptedOut(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{"docbank variable", EnabledEnv, "0"},
		{"generic variable", posthog.GenericEnabledEnv, "0"},
		{"do not track", "DO_NOT_TRACK", "1"},
		{"docbank variable spelled off", EnabledEnv, " off "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableTelemetryEnv(t)
			t.Setenv(tc.env, tc.value)
			dir := t.TempDir()
			reporter := New(Options{Dir: dir, Version: "v1.2.3", Logger: discardLogger()})
			assert.False(t, reporter.Enabled())
			for _, event := range []string{EventAppOpened, EventDaemonActive} {
				assert.True(t, reporter.EventAllowed(event), event)
			}
			handler := CaptureHandler(reporter, dir)
			rec := postEvent(t, handler, `{"event":"app_opened"}`)
			assert.Equal(t, http.StatusAccepted, rec.Code)
			assert.JSONEq(t, `{"status":"disabled"}`, rec.Body.String())
			assert.JSONEq(t, `{"status":"disabled"}`, postEvent(t, handler, screenBody("browse", "web")).Body.String())
			rec = postEvent(t, handler, `{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`)
			assert.Equal(t, http.StatusAccepted, rec.Code)
			assert.JSONEq(t, `{"status":"disabled"}`, rec.Body.String())
			assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, `{"event":"document_added"}`).Code)
			assert.NoFileExists(t, filepath.Join(dir, screenClaimsFile))
			assert.Equal(t, http.StatusBadRequest, postEvent(t, handler, screenBody("unknown", "web")).Code)
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
	r := New(Options{Dir: dir, Version: "v1.2.3", Commit: "test-commit", Logger: discardLogger(), apiKey: testAPIKey, endpoint: srv.URL})
	require.True(t, r.Enabled())
	handler := CaptureHandler(r, dir)
	rec := postEvent(t, handler, `{"event":"app_opened","properties":{"path":"/synthetic/report.pdf","query":"synthetic"}}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.JSONEq(t, `{"status":"queued"}`, rec.Body.String())
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
			assert.Equal(t, "v1.2.3", props["version"])
			assert.Equal(t, "test-commit", props["commit"])
			assert.Equal(t, runtime.GOOS, props["goos"])
			assert.Equal(t, runtime.GOARCH, props["goarch"])
			assert.Empty(t, props["distribution"], "a build without the distribution ldflag reports an empty channel")
			assert.ElementsMatch(t, []string{
				"application", "source", "version", "commit", "goos", "goarch", "install_age_hours", "distribution",
				"$lib", "$lib_version", "$process_person_profile", "$geoip_disable",
			}, slices.Collect(maps.Keys(props)))
		}
	}
	assert.ElementsMatch(t, []string{EventAppOpened, EventDaemonActive}, events)
}

func TestNewFallsBackWhenInstallUnusable(t *testing.T) {
	enableTelemetryEnv(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("synthetic"), 0o600))
	var reporter *Reporter
	require.NotPanics(t, func() { reporter = New(Options{Dir: blocker, Version: "v1.2.3", Logger: discardLogger()}) })
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
	reporter := New(Options{Dir: t.TempDir(), Version: "v1.2.3", Logger: logger, apiKey: testAPIKey, endpoint: server.URL})
	require.True(t, reporter.Enabled())
	require.NoError(t, reporter.Capture(EventDaemonActive, nil))
	require.NoError(t, reporter.Close())
	assert.Contains(t, logs.String(), "synthetic ingest rejection")
	assert.Contains(t, logs.String(), `"daemon":"synthetic"`)
	assert.Contains(t, logs.String(), `"component":"posthog"`)
}
