package api_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
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

type collectedTelemetryEvent struct {
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
}

func newTelemetryCollector(t *testing.T) (string, func() []collectedTelemetryEvent) {
	t.Helper()
	var mu sync.Mutex
	var events []collectedTelemetryEvent
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Batch []collectedTelemetryEvent `json:"batch"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &batch))
		mu.Lock()
		events = append(events, batch.Batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	return collector.URL, func() []collectedTelemetryEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]collectedTelemetryEvent(nil), events...)
	}
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
		{"unknown event", http.MethodPost, url, `{"event":"search_run"}`, nil, http.StatusBadRequest, ""},
		{"blank event", http.MethodPost, url, `{"event":""}`, nil, http.StatusBadRequest, ""},
		{"not json", http.MethodPost, url, `not json`, nil, http.StatusBadRequest, ""},
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

func TestWebSessionRevokeReportsFinalDuration(t *testing.T) {
	if posthog.ProcessDisabled() {
		t.Skip("built with kit_posthog_disabled")
	}
	for _, tc := range []struct {
		name   string
		bucket string
		master bool
		want   bool
	}{
		{name: "one to five minutes", bucket: "1_to_5m", want: true},
		{name: "master key without browser session", bucket: "1_to_5m", master: true},
		{name: "missing bucket"},
		{name: "invalid bucket", bucket: "bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(telemetry.EnabledEnv, "1")
			t.Setenv(posthog.GenericEnabledEnv, "1")
			endpoint, collected := newTelemetryCollector(t)
			reporter, err := posthog.NewReporter(posthog.Options{
				APIKey: "synthetic-api-key", Endpoint: endpoint, Application: "docbank", EnvPrefix: "DOCBANK", DistinctID: "synthetic-install",
			}, posthog.WithAllowedEvent(telemetry.EventSessionEnded,
				posthog.AllowProperty("surface", posthog.AllowStringValues("web")),
				posthog.AllowProperty("duration_bucket", posthog.AllowStringValues("under_1m", "1_to_5m", "5_to_30m", "over_30m"))))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			ts, _ := newTestServer(t, func(d *api.Deps) {
				d.TelemetryReporter = reporter
			})
			session := issueWebSession(t, ts)
			browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session}
			if tc.master {
				browser = nil
			}
			url := ts.URL + "/api/daemon/web-session"
			if tc.bucket != "" {
				url += "?duration_bucket=" + tc.bucket
			}
			status, body := postTelemetryEvent(t, ts.Client(), http.MethodDelete, url, "", browser)
			require.Equal(t, http.StatusNoContent, status, body)
			status, body = postTelemetryEvent(t, ts.Client(), http.MethodDelete, url, "", browser)
			if tc.master {
				require.Equal(t, http.StatusNoContent, status, body)
			} else {
				require.Equal(t, http.StatusUnauthorized, status, body)
			}
			require.NoError(t, reporter.Close())
			events := collected()
			if !tc.want {
				require.Empty(t, events)
				return
			}
			require.Len(t, events, 1)
			assert.Equal(t, "session_ended", events[0].Event)
			assert.Equal(t, "web", events[0].Properties["surface"])
			assert.Equal(t, tc.bucket, events[0].Properties["duration_bucket"])
		})
	}
}

func TestWebSessionRevokeWhenTelemetryOptedOut(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "0")
	reporter := telemetry.New(telemetry.Options{Dir: t.TempDir()})
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	ts, _ := newTestServer(t, func(d *api.Deps) { d.TelemetryReporter = reporter })
	session := issueWebSession(t, ts)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session}
	url := ts.URL + "/api/daemon/web-session?duration_bucket=1_to_5m"
	status, body := postTelemetryEvent(t, ts.Client(), http.MethodDelete, url, "", browser)
	require.Equal(t, http.StatusNoContent, status, body)
	status, body = postTelemetryEvent(t, ts.Client(), http.MethodDelete, url, "", browser)
	assert.Equal(t, http.StatusUnauthorized, status, body)
}

func TestScreenViewedTelemetryEventRoute(t *testing.T) {
	if posthog.ProcessDisabled() {
		t.Skip("built with kit_posthog_disabled")
	}
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(posthog.GenericEnabledEnv, "1")
	endpoint, collected := newTelemetryCollector(t)
	dir := t.TempDir()
	inst, err := posthog.LoadOrCreateInstall(dir)
	require.NoError(t, err)
	reporter, err := posthog.NewReporter(posthog.Options{
		APIKey: "synthetic-api-key", Endpoint: endpoint, Application: "docbank", EnvPrefix: "DOCBANK",
		DistinctID: inst.ID, InstalledAt: inst.InstalledAt, Source: "daemon",
	}, posthog.WithAllowedEvent(telemetry.EventScreenViewed,
		posthog.AllowProperty("screen", posthog.AllowStringValues("browse")),
		posthog.AllowProperty("surface", posthog.AllowStringValues("web"))))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	ts, _ := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = telemetry.CaptureHandler(reporter, dir) })
	session := issueWebSession(t, ts)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session}
	for range 2 {
		status, body := postTelemetryEvent(t, ts.Client(), http.MethodPost, ts.URL+"/api/daemon/telemetry/events",
			`{"event":"screen_viewed","properties":{"screen":"browse","surface":"web"}}`, browser)
		require.Equal(t, http.StatusAccepted, status, body)
	}
	require.NoError(t, reporter.Close())
	events := collected()
	require.Len(t, events, 1)
	assert.Equal(t, telemetry.EventScreenViewed, events[0].Event)
	assert.Equal(t, "browse", events[0].Properties["screen"])
}
