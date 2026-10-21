package api_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/telemetry"
)

func postTelemetryEvent(t *testing.T, client *http.Client, method, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

func TestTelemetryEventRoute(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "0")
	reporter := telemetry.New(telemetry.Options{Dir: t.TempDir()})
	ts, _ := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = telemetry.CaptureHandler(reporter, t.TempDir()) })
	session := issueWebSession(t, ts)
	url := ts.URL + "/api/daemon/telemetry/events"
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session}

	for _, tc := range []struct {
		name    string
		method  string
		url     string
		body    string
		headers map[string]string
		status  int
		want    string
	}{
		{"master key", http.MethodPost, url, `{"event":"app_opened"}`, nil, http.StatusAccepted, `"status":"disabled"`},
		{"browser session", http.MethodPost, url, `{"event":"app_opened"}`, browser, http.StatusAccepted, `"status":"disabled"`},
		{"no credentials", http.MethodPost, url, `{"event":"app_opened"}`, map[string]string{"X-Api-Key": ""}, http.StatusUnauthorized, ""},
		{"browser session with query", http.MethodPost, url + "?x=1", `{"event":"app_opened"}`, browser, http.StatusForbidden, `"code":"web_session_read_only"`},
		{"get", http.MethodGet, url, "", nil, http.StatusMethodNotAllowed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postTelemetryEvent(t, ts.Client(), tc.method, tc.url, tc.body, tc.headers)
			assert.Equal(t, tc.status, status, body)
			if tc.want != "" {
				assert.Contains(t, body, tc.want)
			}
		})
	}
}

func TestTelemetryEventRouteAbsentWithoutCapture(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	status, body := postTelemetryEvent(t, ts.Client(), http.MethodPost, ts.URL+"/api/daemon/telemetry/events", `{"event":"app_opened"}`, nil)
	assert.Equal(t, http.StatusNotFound, status, body)
}

type recordingTelemetryCapture struct {
	http.Handler

	mu         sync.Mutex
	events     []string
	properties []map[string]any
	err        error
}

func (c *recordingTelemetryCapture) Capture(event string, properties map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	c.properties = append(c.properties, properties)
	return c.err
}

func TestWebSessionRevokeReportsFinalDuration(t *testing.T) {
	t.Parallel()
	logs, err := os.CreateTemp(t.TempDir(), "telemetry-*.json")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, logs.Close()) })
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, tc := range []struct {
		name   string
		bucket string
		master bool
		want   bool
		fails  bool
		query  string
	}{
		{name: "one to five minutes", bucket: "1_to_5m", query: "duration_bucket=1_to_5m&extra=1", want: true},
		{name: "master key without browser session", bucket: "1_to_5m", master: true},
		{name: "missing bucket"},
		{name: "invalid bucket", query: "duration_bucket=bogus&extra=1"},
		{name: "repeated bucket", query: "duration_bucket=1_to_5m&duration_bucket=over_30m"},
		{name: "capture failure", bucket: "over_30m", fails: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &recordingTelemetryCapture{Handler: http.NotFoundHandler()}
			if tc.fails {
				capture.err = errors.New("synthetic capture failure")
			}
			ts, _ := newTestServer(t, func(d *api.Deps) {
				d.Logger = logger
				d.TelemetryCapture = capture
			})
			session := issueWebSession(t, ts)
			browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session}
			if tc.master {
				browser = nil
			}
			url := ts.URL + "/api/daemon/web-session"
			if tc.query != "" {
				url += "?" + tc.query
			} else if tc.bucket != "" {
				url += "?duration_bucket=" + tc.bucket
			}
			status, body := postTelemetryEvent(t, ts.Client(), http.MethodDelete, url, "", browser)
			require.Equal(t, http.StatusNoContent, status, body)
			capture.mu.Lock()
			defer capture.mu.Unlock()
			if !tc.want {
				require.Empty(t, capture.events)
				return
			}
			assert.Equal(t, []string{"session_ended"}, capture.events)
			assert.Equal(t, []map[string]any{{"surface": "web", "duration_bucket": tc.bucket}}, capture.properties)
			if tc.fails {
				data, err := os.ReadFile(logs.Name())
				require.NoError(t, err)
				assert.Contains(t, string(data), `"level":"DEBUG","msg":"final session_ended capture failed"`)
				assert.Contains(t, string(data), "synthetic capture failure")
			}
		})
	}
}
