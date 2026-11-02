package posthog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureHandlerTestOption() Option {
	return WithAllowedEvent("app_opened",
		AllowProperty("view", AllowStringValues("sessions", "search")),
		AllowProperty("count", AllowNumber),
	)
}

func postCapture(t *testing.T, h http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postCaptureAs(t, h, method, "application/json", body)
}

func postCaptureAs(
	t *testing.T, h http.Handler, method, contentType, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, "/telemetry/events", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var resp postHogCaptureResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp.Status
}

// newOptedOutCaptureTestReporter builds a reporter opted out by the prefixed
// environment variable and returns how often its client factory ran.
func newOptedOutCaptureTestReporter(t *testing.T) (*Reporter, *int) {
	t.Helper()
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "0")
	factoryCalls := 0
	reporter, err := newPostHogReporter(Options{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
	}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
		factoryCalls++
		return &recordingPostHogClient{}, nil
	}, captureHandlerTestOption())
	require.NoError(t, err)
	return reporter, &factoryCalls
}

type captureHandlerTestState struct {
	name  string
	build func(t *testing.T) (reporter *Reporter, sent func() int, factoryCalls func() int)
}

func optedOutCaptureState() captureHandlerTestState {
	return captureHandlerTestState{name: "env_opt_out", build: func(t *testing.T) (*Reporter, func() int, func() int) {
		t.Helper()
		reporter, calls := newOptedOutCaptureTestReporter(t)
		return reporter, func() int { return 0 }, func() int { return *calls }
	}}
}

func processDisabledCaptureState() captureHandlerTestState {
	return captureHandlerTestState{name: "process_disabled", build: func(t *testing.T) (*Reporter, func() int, func() int) {
		t.Helper()
		reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
		DisableProcess()
		t.Cleanup(enablePostHogTelemetryForTest)
		return reporter, func() int { return len(client.messages) }, nil
	}}
}

func closedCaptureState() captureHandlerTestState {
	return captureHandlerTestState{name: "closed", build: func(t *testing.T) (*Reporter, func() int, func() int) {
		t.Helper()
		reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
		require.NoError(t, reporter.Close())
		return reporter, func() int { return len(client.messages) }, nil
	}}
}

func TestPostHogCaptureHandlerCapturesAllowedEventWithAllowedProperties(t *testing.T) {
	reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart.Add(-30*time.Hour), captureHandlerTestOption())

	rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost,
		`{"event":"app_opened","properties":{"view":"sessions","count":2,"path":"/Users/example/private","query":"secret"}}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "queued", decodeStatus(t, rec))
	require.Len(t, client.messages, 1)
	message := client.messages[0]
	assert.Equal(t, "app_opened", message.Event)
	assert.Equal(t, reporter.distinctID, message.DistinctId)
	assert.Equal(t, "sessions", message.Properties["view"])
	assert.IsType(t, float64(0), message.Properties["count"])
	assert.InDelta(t, 2, message.Properties["count"], 0)
	assert.NotContains(t, message.Properties, "path")
	assert.NotContains(t, message.Properties, "query")
	assert.Equal(t, int64(30), message.Properties[postHogInstallAgeProperty])
	assert.Equal(t, reporter.application, message.Properties["application"])
}

func TestPostHogCaptureHandlerRejectEventsOnlyRestrictsConfiguredHandler(t *testing.T) {
	reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart,
		captureHandlerTestOption(), WithAllowedEvent("background_ready"), WithAllowedEvent("archive_ready"))
	handler := NewCaptureHandler(reporter, RejectEvents("background_ready"), RejectEvents("archive_ready"))
	for _, event := range []string{"background_ready", " background_ready ", "archive_ready"} {
		rec := postCapture(t, handler, http.MethodPost, `{"event":"`+event+`"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, ErrUnsupportedEvent.Error()+"\n", rec.Body.String())
	}
	assert.Empty(t, client.messages)

	rec := postCapture(t, handler, http.MethodPost, `{"event":"app_opened"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "queued", decodeStatus(t, rec))

	rec = postCapture(t, NewCaptureHandler(reporter), http.MethodPost, `{"event":"background_ready"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "queued", decodeStatus(t, rec))

	status, err := reporter.Report(t.Context(), "archive_ready", nil)
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, status)
	require.Len(t, client.messages, 3)
	assert.Equal(t, "app_opened", client.messages[0].Event)
	assert.Equal(t, "background_ready", client.messages[1].Event)
	assert.Equal(t, "archive_ready", client.messages[2].Event)
}

func TestPostHogCaptureHandlerRejectEventsPreservesRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		code        int
		message     string
	}{
		{"method", http.MethodGet, "text/plain", `{"event":"app_opened"}`, http.StatusMethodNotAllowed, "method not allowed\n"},
		{"content_type", http.MethodPost, "text/plain", `{"event":"app_opened"}`, http.StatusUnsupportedMediaType, "telemetry request must be application/json\n"},
		{"oversized_trailing_space", http.MethodPost, "application/json", `{"event":"app_opened"}` + strings.Repeat(" ", 64<<10), http.StatusRequestEntityTooLarge, "telemetry request too large\n"},
		{"trailing_data", http.MethodPost, "application/json", `{"event":"app_opened"} trailing`, http.StatusBadRequest, "invalid telemetry request\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
			handler := NewCaptureHandler(reporter, RejectEvents("app_opened"))
			rec := postCaptureAs(t, handler, tt.method, tt.contentType, tt.body)
			assert.Equal(t, tt.code, rec.Code)
			assert.Equal(t, tt.message, rec.Body.String())
			assert.Empty(t, client.messages)
		})
	}
}

func TestPostHogCaptureHandlerRejectsEventsInEveryState(t *testing.T) {
	states := []captureHandlerTestState{
		{name: "enabled", build: func(t *testing.T) (*Reporter, func() int, func() int) {
			t.Helper()
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
			return reporter, func() int { return len(client.messages) }, nil
		}},
		optedOutCaptureState(),
		processDisabledCaptureState(),
		closedCaptureState(),
		{name: "disabled_reporter", build: func(*testing.T) (*Reporter, func() int, func() int) {
			return DisabledReporter(), func() int { return 0 }, nil
		}},
		{name: "nil_reporter", build: func(*testing.T) (*Reporter, func() int, func() int) {
			return nil, func() int { return 0 }, nil
		}},
	}
	bodies := []string{
		`{"event":"app_closed"}`,
		`{"event":"   "}`,
		`{"event":""}`,
		`{"properties":{"view":"sessions"}}`,
		`null`,
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			reporter, sent, factoryCalls := state.build(t)
			for _, body := range bodies {
				rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost, body)
				assert.Equal(t, http.StatusBadRequest, rec.Code, body)
			}
			handler := NewCaptureHandler(reporter, RejectEvents(" app_opened "))
			for _, body := range []string{`{"event":"app_opened"}`} {
				rec := postCapture(t, handler, http.MethodPost, body)
				assert.Equal(t, http.StatusBadRequest, rec.Code, body)
				assert.Equal(t, ErrUnsupportedEvent.Error()+"\n", rec.Body.String(), body)
			}
			assert.Zero(t, sent())
			if factoryCalls != nil {
				assert.Zero(t, factoryCalls())
			}
		})
	}
}

func TestPostHogCaptureHandlerDisabledReporterAnswersDisabled(t *testing.T) {
	states := []captureHandlerTestState{
		optedOutCaptureState(),
		processDisabledCaptureState(),
		closedCaptureState(),
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			reporter, sent, factoryCalls := state.build(t)

			rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost,
				`{"event":"app_opened","properties":{"view":"sessions"}}`)

			require.Equal(t, http.StatusAccepted, rec.Code)
			assert.Equal(t, "disabled", decodeStatus(t, rec))
			assert.Zero(t, sent())
			if factoryCalls != nil {
				assert.Zero(t, factoryCalls())
			}
		})
	}
}

func TestPostHogCaptureHandlerKeepsReporterOwnedProperties(t *testing.T) {
	reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart.Add(-30*time.Hour),
		WithAllowedEvent("app_opened",
			AllowProperty("application", allowAnyTelemetryValue),
			AllowProperty("version", allowAnyTelemetryValue),
			AllowProperty("source", allowAnyTelemetryValue),
			AllowProperty(postHogInstallAgeProperty, allowAnyTelemetryValue),
			AllowProperty("$process_person_profile", allowAnyTelemetryValue),
			AllowProperty("$geoip_disable", allowAnyTelemetryValue),
			AllowProperty("goos", allowAnyTelemetryValue),
		))

	rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost,
		`{"event":"app_opened","properties":{"application":"spoof","version":"spoof","source":"browser","install_age_hours":0,"$process_person_profile":true,"$geoip_disable":false,"goos":"plan9"}}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "queued", decodeStatus(t, rec))
	require.Len(t, client.messages, 1)
	message := client.messages[0]
	assert.Equal(t, reporter.distinctID, message.DistinctId)
	assert.Equal(t, reporter.application, message.Properties["application"])
	assert.Equal(t, reporter.version, message.Properties["version"])
	assert.Equal(t, reporter.source, message.Properties["source"])
	assert.Equal(t, int64(30), message.Properties[postHogInstallAgeProperty])
	assert.Equal(t, false, message.Properties["$process_person_profile"])
	assert.Equal(t, true, message.Properties["$geoip_disable"])
	assert.Equal(t, runtime.GOOS, message.Properties["goos"])
}

func TestPostHogCaptureHandlerDropsUnsafePropertyValues(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
	}{
		{name: "path_view", body: `{"event":"app_opened","properties":{"view":"bad/path"}}`, key: "view"},
		{name: "array_view", body: `{"event":"app_opened","properties":{"view":["sessions"]}}`, key: "view"},
		{name: "string_count", body: `{"event":"app_opened","properties":{"count":"3"}}`, key: "count"},
		{name: "object_count", body: `{"event":"app_opened","properties":{"count":{"n":3}}}`, key: "count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost, tt.body)

			require.Equal(t, http.StatusAccepted, rec.Code)
			assert.Equal(t, "queued", decodeStatus(t, rec))
			require.Len(t, client.messages, 1)
			assert.NotContains(t, client.messages[0].Properties, tt.key)
		})
	}
}

func TestPostHogCaptureHandlerRejectsMalformedBody(t *testing.T) {
	bodies := []string{
		`{"event":`,
		`[]`,
		`{"event":"app_opened","properties":"view"}`,
		`{"event":7}`,
		`{"event":"app_opened"} trailing`,
		`{"event":"app_opened"}{"event":"app_opened"}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost, body)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Empty(t, client.messages)
		})
	}
}

func TestPostHogCaptureHandlerRejectsNonPost(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCapture(t, NewCaptureHandler(reporter), method, `{"event":"app_opened"}`)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
			assert.Empty(t, client.messages)
		})
	}
}

func TestPostHogCaptureHandlerReportsEnqueueFailure(t *testing.T) {
	reporter, _ := newInstallAgeTestReporterWithClient(t,
		failingPostHogClient{enqueueErr: phsdk.ErrQueueFull}, postHogTestStart, captureHandlerTestOption())

	rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost, `{"event":"app_opened"}`)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "queued")
}

func TestPostHogCaptureHandlerRequiresJSONContentType(t *testing.T) {
	contentTypes := []string{
		"",
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"application/json; charset",
	}
	for _, contentType := range contentTypes {
		t.Run(contentType, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCaptureAs(t, NewCaptureHandler(reporter), http.MethodPost, contentType,
				`{"event":"app_opened"}`)

			assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
			assert.Empty(t, client.messages)
		})
	}
}

func TestPostHogCaptureHandlerAcceptsJSONContentTypeVariants(t *testing.T) {
	for _, contentType := range []string{"application/json; charset=utf-8", "Application/JSON"} {
		t.Run(contentType, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCaptureAs(t, NewCaptureHandler(reporter), http.MethodPost, contentType,
				"{\"event\":\"app_opened\"}\n")

			require.Equal(t, http.StatusAccepted, rec.Code)
			assert.Equal(t, "queued", decodeStatus(t, rec))
			assert.Len(t, client.messages, 1)
		})
	}
}

func TestPostHogCaptureHandlerRejectsOversizedBody(t *testing.T) {
	reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
	body := `{"event":"app_opened","properties":{"view":"` + strings.Repeat("a", 64<<10) + `"}}`

	rec := postCapture(t, NewCaptureHandler(reporter), http.MethodPost, body)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Empty(t, client.messages)
}
