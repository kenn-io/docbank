package api_test

import (
	"io"
	"net/http"
	"strings"
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
	ts, _ := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = nil })
	status, body := postTelemetryEvent(t, ts.Client(), http.MethodPost, ts.URL+"/api/daemon/telemetry/events", `{"event":"document_opened","properties":{"surface":"web"}}`, nil)
	assert.Equal(t, http.StatusNotFound, status, body)
}

func TestScreenViewedTelemetryEventRoute(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(posthog.GenericEnabledEnv, "1")
	capture := newActionCollector(t)
	ts, _ := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = telemetry.CaptureHandler(capture.reporter, t.TempDir()) })
	session := issueWebSession(t, ts)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session}
	for range 2 {
		status, body := postTelemetryEvent(t, ts.Client(), http.MethodPost, ts.URL+"/api/daemon/telemetry/events",
			`{"event":"screen_viewed","properties":{"screen":"browse","surface":"web"}}`, browser)
		require.Equal(t, http.StatusAccepted, status, body)
	}
	capture.requireEvent(t, "screen_viewed", map[string]any{"screen": "browse"})
}
