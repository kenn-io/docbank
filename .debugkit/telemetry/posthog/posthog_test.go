package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	for _, env := range []string{"CI", "DO_NOT_TRACK", GenericEnabledEnv, "KATA_TELEMETRY_ENABLED", "TEST_TELEMETRY_ENABLED"} {
		if err := os.Unsetenv(env); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

type fakePostHogClient struct {
	message phsdk.Message
	closed  bool
}

func (f *fakePostHogClient) Enqueue(message phsdk.Message) error {
	f.message = message
	return nil
}

func (*fakePostHogClient) FlushWithContext(context.Context) error { return nil }

func (f *fakePostHogClient) Close() error {
	f.closed = true
	return nil
}

type blockingPostHogClient struct {
	message        phsdk.Message
	enqueueStarted chan struct{}
	unblockEnqueue chan struct{}
	closeCalled    atomic.Bool
}

func (b *blockingPostHogClient) Enqueue(message phsdk.Message) error {
	b.message = message
	close(b.enqueueStarted)
	<-b.unblockEnqueue
	return nil
}

func (*blockingPostHogClient) FlushWithContext(context.Context) error { return nil }

func (b *blockingPostHogClient) Close() error {
	b.closeCalled.Store(true)
	return nil
}

func TestPrefixedTelemetryEnabledEnv(t *testing.T) {
	assert.Equal(t, "KATA_TELEMETRY_ENABLED", PrefixedEnabledEnv(" kata "))
	assert.Equal(t, "ROBOREV_TELEMETRY_ENABLED", PrefixedEnabledEnv("ROBOREV"))
	assert.Empty(t, PrefixedEnabledEnv(""))
}

func TestEligible(t *testing.T) {
	type admissionCase struct {
		name, generic, prefixed string
		absent                  bool
		veto, value             string
		version                 string
		processDisabled         bool
		enabled                 bool
	}
	tests := []admissionCase{
		{name: "absent", version: "v1.2.3", absent: true, enabled: true},
		{name: "empty", version: "v1.2.3", enabled: true},
		{name: "unknown", version: "v1.2.3", generic: "enabled", prefixed: "2", enabled: true},
		{name: "tracking zero", version: "v1.2.3", absent: true, veto: "DO_NOT_TRACK", value: "0", enabled: true},
		{name: "tracking false", version: "v1.2.3", absent: true, veto: "DO_NOT_TRACK", value: " FALSE ", enabled: true},
		{name: "tracking padded zero", version: "v1.2.3", absent: true, veto: "DO_NOT_TRACK", value: " 0 ", enabled: true},
		{name: "tracking set", version: "v1.2.3", absent: true, veto: "DO_NOT_TRACK", value: "1"},
		{name: "tracking empty", version: "v1.2.3", absent: true, veto: "DO_NOT_TRACK"},
		{name: "tracking other", version: "v1.2.3", generic: "1", veto: "DO_NOT_TRACK", value: "off"},
		{name: "ci set", version: "v1.2.3", absent: true, veto: "CI", value: "true"},
		{name: "ci empty", version: "v1.2.3", generic: "1", veto: "CI"},
		{name: "ci false", version: "v1.2.3", generic: "1", veto: "CI", value: "false"},
		{name: "process disable", version: "v1.2.3", generic: "1", processDisabled: true},
		{name: "release without v", absent: true, version: "1.2.3", enabled: true},
		{name: "development", absent: true, version: "dev"},
		{name: "fork", absent: true, version: "v1.2.3-fork.1"},
		{name: "empty version", absent: true},
		{name: "partial version", absent: true, version: "v1.2"},
		{name: "build metadata", absent: true, version: "v1.2.3+build.1"},
		{name: "leading space", absent: true, version: " v1.2.3"},
		{name: "trailing newline", absent: true, version: "v1.2.3\n"},
	}
	for _, value := range []string{"1", "true", "yes", "on", " TRUE ", " YeS ", " ON ", " 1 "} {
		tests = append(tests,
			admissionCase{name: "generic " + value, version: "v1.2.3", generic: value, enabled: true},
			admissionCase{name: "prefixed " + value, version: "v1.2.3", prefixed: value, enabled: true})
	}
	for _, value := range []string{"0", "false", "no", "off", " FALSE ", "Off"} {
		tests = append(tests,
			admissionCase{name: "generic " + value, version: "v1.2.3", generic: value, prefixed: "1"},
			admissionCase{name: "prefixed " + value, version: "v1.2.3", generic: "1", prefixed: value})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enablePostHogTelemetryForTest()
			t.Cleanup(enablePostHogTelemetryForTest)
			if !tt.absent {
				t.Setenv(GenericEnabledEnv, tt.generic)
				t.Setenv("KATA_TELEMETRY_ENABLED", tt.prefixed)
			}
			if tt.veto != "" {
				t.Setenv(tt.veto, tt.value)
			}
			if tt.processDisabled {
				DisableProcess()
			}
			assert.Equal(t, tt.enabled, Eligible("KATA", tt.version))
		})
	}
}

func TestReporterCloseGivesUpAfterShutdownTimeout(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	// A server that never answers stands in for a network that drops packets.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	reporter, err := NewReporter(Options{
		APIKey: "phc_test", Endpoint: server.URL, Application: "kata",
		EnvPrefix: "KATA", DistinctID: "anonymous-instance-id", Version: "v1.2.3",
	}, WithAllowedEvent("daemon_active"))
	require.NoError(t, err)
	require.NoError(t, reporter.Capture("daemon_active", nil))

	start := time.Now()
	err = reporter.Close()

	require.Error(t, err)
	assert.Less(t, time.Since(start), ShutdownTimeout+time.Second)
	assert.False(t, reporter.Enabled())
}

func TestNewPostHogReporterDisabledByEnvSkipsRequiredFields(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "0")

	reporter, err := NewReporter(Options{Version: "v1.2.3"})

	require.NoError(t, err)
	assert.False(t, reporter.Enabled())
}

func TestReporterRequiresReleaseVersion(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	for _, tt := range []struct {
		name    string
		version string
		enabled bool
	}{
		{name: "release", version: "v1.2.3", enabled: true},
		{name: "fork", version: "v1.2.3-fork.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := &recordingPostHogClient{}
			reporter, err := newPostHogReporter(Options{
				APIKey: "caller-owned-key", Application: "kata", EnvPrefix: "KATA",
				DistinctID: "anonymous-instance-id", Version: tt.version,
			}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
				calls++
				return client, nil
			}, WithAllowedEvent(EventDaemonActive))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			assert.Equal(t, tt.enabled, reporter.Enabled())
			require.NoError(t, reporter.Capture(EventDaemonActive, nil))
			if tt.enabled {
				assert.Equal(t, 1, calls)
				require.Len(t, client.messages, 1)
				assert.Equal(t, EventDaemonActive, client.messages[0].Event)
				return
			}
			assert.Zero(t, calls)
			assert.Empty(t, client.messages)
		})
	}
}

func TestNewPostHogReporterDisabledRetainsAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		disable func(t *testing.T)
	}{
		{name: "generic_env", disable: func(t *testing.T) {
			t.Helper()
			t.Setenv(GenericEnabledEnv, " 0 ")
			t.Setenv("KATA_TELEMETRY_ENABLED", "1")
		}},
		{name: "prefixed_env", disable: func(t *testing.T) {
			t.Helper()
			t.Setenv(GenericEnabledEnv, "1")
			t.Setenv("KATA_TELEMETRY_ENABLED", " 0")
		}},
		{name: "process_disable", disable: func(t *testing.T) {
			t.Helper()
			t.Setenv(GenericEnabledEnv, "1")
			t.Setenv("KATA_TELEMETRY_ENABLED", "1")
			DisableProcess()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enablePostHogTelemetryForTest()
			t.Cleanup(enablePostHogTelemetryForTest)
			tt.disable(t)
			options := []Option{nil, WithAllowedEvent("app_opened")}

			nilFactoryReporter, err := newPostHogReporter(Options{EnvPrefix: "KATA", Version: "v1.2.3"}, nil, options...)
			require.NoError(t, err)
			assert.True(t, nilFactoryReporter.EventAllowed("app_opened"))

			factoryCalls := 0
			reporter, err := newPostHogReporter(Options{EnvPrefix: "KATA", Version: "v1.2.3"}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
				factoryCalls++
				return &recordingPostHogClient{}, nil
			}, options...)
			require.NoError(t, err)

			assert.False(t, reporter.Enabled())
			assert.True(t, reporter.EventAllowed("app_opened"))
			assert.False(t, reporter.EventAllowed("app_closed"))
			require.NoError(t, reporter.Capture("app_closed", nil))
			assert.Zero(t, factoryCalls)
		})
	}
}

func TestNewPostHogReporterProjectKey(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	for _, tt := range []struct {
		name       string
		apiKey     string
		linkKey    string
		wantAPIKey string
	}{
		{name: "no key disables reporting"},
		{name: "blank keys disable reporting", apiKey: " ", linkKey: " "},
		{name: "link-time key", linkKey: "link-time-key", wantAPIKey: "link-time-key"},
		{name: "caller key wins", apiKey: "caller-owned-key", linkKey: "link-time-key", wantAPIKey: "caller-owned-key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previous := projectKey
			projectKey = tt.linkKey
			t.Cleanup(func() { projectKey = previous })

			var gotAPIKeys []string
			client := &recordingPostHogClient{}
			reporter, err := newPostHogReporter(Options{
				APIKey:      tt.apiKey,
				Application: "kata",
				EnvPrefix:   "KATA",
				DistinctID:  "anonymous-instance-id",
				Version:     "v1.2.3",
			}, func(apiKey string, _ phsdk.Config) (postHogEnqueueCloser, error) {
				gotAPIKeys = append(gotAPIKeys, apiKey)
				return client, nil
			}, WithAllowedEvent(EventDaemonActive))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			require.NoError(t, reporter.Capture(EventDaemonActive, nil))

			if tt.wantAPIKey == "" {
				assert.False(t, reporter.Enabled())
				assert.Empty(t, gotAPIKeys)
				assert.Empty(t, client.messages)
				return
			}
			assert.True(t, reporter.Enabled())
			assert.Equal(t, []string{tt.wantAPIKey}, gotAPIKeys)
			assert.Len(t, client.messages, 1)
		})
	}
}

func TestNewPostHogReporterPassesMandatoryAPIKeyAndEndpointToPostHog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	var gotAPIKey string
	var gotConfig phsdk.Config
	reporter, err := newPostHogReporter(Options{
		APIKey:      "caller-owned-key",
		Endpoint:    "https://posthog.example.test",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		Version:     "v1.2.3",
	}, func(apiKey string, config phsdk.Config) (postHogEnqueueCloser, error) {
		gotAPIKey = apiKey
		gotConfig = config
		return &fakePostHogClient{}, nil
	}, testAllowedTelemetryOptions()...)

	require.NoError(err)
	require.True(reporter.Enabled())
	assert.Equal("caller-owned-key", gotAPIKey)
	assert.Equal("https://posthog.example.test", gotConfig.Endpoint)
	assert.Equal(phsdk.CaptureModeLegacy, gotConfig.CaptureMode)
	require.NotNil(gotConfig.DisableGeoIP)
	assert.True(*gotConfig.DisableGeoIP)
}

func TestPostHogReporterCaptureUsesAnonymousDistinctIDAndPrivacyDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		version:       "v-test",
		commit:        "abc123",
		source:        "daemon",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("daemon_started", map[string]any{
		"$geoip_disable":          false,
		"$process_person_profile": true,
		"application":             "caller-app",
		"distinct_id":             "user-provided",
		"path":                    "/Users/example/private",
		"project_count":           3,
		"sync_enabled":            true,
		"view":                    "dashboard",
	})

	require.NoError(err)
	capture, ok := client.message.(phsdk.Capture)
	require.True(ok)
	assert.Equal("anonymous-instance-id", capture.DistinctId)
	assert.Equal("daemon_started", capture.Event)
	assert.Equal(3, capture.Properties["project_count"])
	assert.Equal(true, capture.Properties["sync_enabled"])
	assert.NotContains(capture.Properties, "distinct_id")
	assert.NotContains(capture.Properties, "path")
	assert.NotContains(capture.Properties, "view")
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
	assert.Equal("kata", capture.Properties["application"])
	assert.Equal("v-test", capture.Properties["version"])
	assert.Equal("abc123", capture.Properties["commit"])
	assert.Equal(runtime.GOOS, capture.Properties["goos"])
	assert.Equal(runtime.GOARCH, capture.Properties["goarch"])
	assert.Equal("daemon", capture.Properties["source"])
}

func TestPostHogReporterCaptureRejectsUnsupportedEvents(t *testing.T) {
	reporter := &Reporter{
		client:        &fakePostHogClient{},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("issue_created", map[string]any{"project_count": 1})

	require.ErrorIs(t, err, ErrUnsupportedEvent)
}

func TestPostHogReporterCaptureDropsUnsafePropertyValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		source:        "daemon",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("daemon_active", map[string]any{
		"project_count": "private-project-name",
		"sync_enabled":  "yes",
		"view":          "bad/path",
	})

	require.NoError(err)
	capture, ok := client.message.(phsdk.Capture)
	require.True(ok)
	assert.NotContains(capture.Properties, "project_count")
	assert.NotContains(capture.Properties, "sync_enabled")
	assert.NotContains(capture.Properties, "view")
}

func TestPostHogReporterAllowsDefaultPropertiesOnlyEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	client := &fakePostHogClient{}
	reporter, err := newPostHogReporter(Options{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		Version:     "v1.2.3",
		Commit:      "abc123",
	}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
		return client, nil
	}, WithAllowedEvent("event_without_properties"))
	require.NoError(err)

	err = reporter.Capture("event_without_properties", map[string]any{
		"private_path": "/Users/example/private",
	})
	require.NoError(err)

	capture, ok := client.message.(phsdk.Capture)
	require.True(ok)
	assert.Equal("event_without_properties", capture.Event)
	assert.NotContains(capture.Properties, "private_path")
	assert.Equal("kata", capture.Properties["application"])
	assert.Equal("daemon", capture.Properties["source"])
	assert.Equal("v1.2.3", capture.Properties["version"])
	assert.Equal("abc123", capture.Properties["commit"])
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
}

func TestPostHogReporterCaptureHonorsProcessDisableAfterCreation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		deliveries:    &deliveryCallbacks{},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}
	require.True(reporter.Enabled())

	DisableProcess()
	t.Cleanup(enablePostHogTelemetryForTest)

	assert.False(reporter.Enabled())
	err := reporter.Capture("daemon_active", map[string]any{"project_count": 1})
	require.NoError(err)
	assert.Nil(client.message)

	require.NoError(reporter.Close())
	assert.True(client.closed)
	assert.False(reporter.Enabled())
}

func TestPostHogDisableTransportNoOpsRequestsAfterProcessDisable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)

	baseCalled := false
	transport := postHogDisableTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			baseCalled = true
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}

	DisableProcess()

	resp, err := transport.RoundTrip(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://posthog.example.test/batch", nil))

	require.NoError(err)
	require.NotNil(resp)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusNoContent, resp.StatusCode)
	assert.False(baseCalled)
}

func TestReporterWarnsOnceForSDKProperty(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	var logs bytes.Buffer
	reporter, err := newPostHogReporter(Options{
		APIKey: "phc_test", Application: "test", EnvPrefix: "TEST", DistinctID: "synthetic-install",
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	}, func(string, phsdk.Config) (postHogEnqueueCloser, error) { return &fakePostHogClient{}, nil },
		WithAllowedEvent("screen", AllowProperty("$os", AllowStringValues("one", "two"))),
		WithAllowedEvent("other", AllowProperty("$os", AllowStringValues("one", "two"))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reporter.Close() })
	assert.Equal(t, 1, strings.Count(logs.String(), `"property":"$os"`))
	assert.Contains(t, logs.String(), `"level":"WARN"`)
	assert.Contains(t, logs.String(), `"component":"posthog"`)
}

func TestPostHogTransportAllowsOnlyDocumentedBatchFields(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	for _, tc := range []struct{ name, payload, want string }{
		{"future fields", `{"api_key":"phc_test","future_envelope":true,"batch":[{"event":"screen","future_event":true,"properties":{"application":"test","goos":"linux","$lib":"posthog-go","count":9007199254740993,"other_event_only":true,"$os":"redacted","$machine_class":"workstation"}}]}`, `{"api_key":"phc_test","batch":[{"event":"screen","properties":{"application":"test","goos":"linux","$lib":"posthog-go","count":9007199254740993}}]}`},
		{"missing properties", `{"api_key":"phc_test","batch":[{"event":"screen","future_event":true},{"event":"screen","properties":{"count":9007199254740993,"$machine_class":"workstation"}}]}`, `{"api_key":"phc_test","batch":[{"event":"screen"},{"event":"screen","properties":{"count":9007199254740993}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			transport := postHogDisableTransport{
				allowedEvents: map[string]map[string]PropertyFilter{
					"screen": {"count": AllowNumber, "$os": AllowStringValues("redacted")},
					"other":  {"other_event_only": AllowBool},
				},
				base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					called = true
					defer req.Body.Close()
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.JSONEq(t, tc.want, string(body))
					assert.Contains(t, string(body), "9007199254740993")
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
				}),
			}
			resp, err := transport.RoundTrip(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://posthog.example.test/batch/", strings.NewReader(tc.payload)))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.True(t, called)
		})
	}
}

func TestPostHogTransportPassesNonBatchRequestsThrough(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	for _, tc := range []struct{ name, payload string }{
		{name: "nil body"},
		{name: "non-batch JSON", payload: `{"api_key":"phc_test", "distinct_id":"synthetic-install"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			transport := postHogDisableTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				if tc.payload == "" {
					assert.Nil(t, req.Body)
				} else {
					defer req.Body.Close()
					body, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					assert.Equal(t, tc.payload, string(body))
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://posthog.example.test/flags/", strings.NewReader(tc.payload))
			if tc.payload == "" {
				req.Body = nil
			}
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.True(t, called)
		})
	}
}

func TestPostHogTransportRejectsInvalidBatchBeforeSending(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	for _, tc := range []struct {
		name, payload string
		readErr       error
	}{
		{name: "malformed", payload: `{"batch":`},
		{name: "invalid batch", payload: `{"batch":{}}`},
		{name: "unreadable", readErr: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			transport := postHogDisableTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://posthog.example.test/collect", strings.NewReader(tc.payload))
			if tc.readErr != nil {
				req.Body = io.NopCloser(iotest.ErrReader(tc.readErr))
			}
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				defer resp.Body.Close()
			}
			require.Error(t, err)
			if tc.readErr != nil {
				require.ErrorIs(t, err, tc.readErr)
			}
			assert.Nil(t, resp)
			assert.False(t, called)
		})
	}
}

func TestReporterSerializedPayloadMatchesDocumentedFields(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	previousDistribution := distribution
	distribution = "homebrew"
	t.Cleanup(func() { distribution = previousDistribution })
	for _, tc := range []struct {
		name     string
		redirect int
		paths    []string
	}{
		{name: "direct", paths: []string{"/ingest/batch/"}},
		{name: "redirect 307", redirect: http.StatusTemporaryRedirect, paths: []string{"/ingest/batch/", "/collect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type receivedBatch struct {
				path string
				body []byte
			}
			bodies := make(chan receivedBatch, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assert.Equal(t, http.MethodPost, req.Method)
				body, err := io.ReadAll(req.Body)
				assert.NoError(t, err)
				bodies <- receivedBatch{req.URL.Path, body}
				if tc.redirect != 0 && req.URL.Path != "/collect" {
					http.Redirect(w, req, "/collect", tc.redirect)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
			reporter, err := NewReporter(Options{
				APIKey: "phc_test", Endpoint: server.URL + "/ingest", Application: "test", EnvPrefix: "TEST",
				DistinctID: "synthetic-install", Version: "v1.2.3", Commit: "abc123", Source: "cli",
				InstalledAt: now.Add(-48 * time.Hour),
			}, WithAllowedEvent("screen_view", AllowProperty("screen", AllowStringValues("home")), AllowProperty("count", AllowNumber), AllowProperty("$os", AllowStringValues("redacted")), AllowProperty("$os_version", AllowStringValues("redacted")), AllowProperty("$os_distro", AllowStringValues("redacted")), AllowProperty("$go_version", AllowStringValues("redacted"))),
				postHogOptionFunc(func(config *postHogReporterConfig) { config.now = func() time.Time { return now } }))
			require.NoError(t, err)
			t.Cleanup(func() { _ = reporter.Close() })
			require.NoError(t, reporter.Capture("screen_view", map[string]any{"screen": "home", "count": int64(9007199254740993), "$os": "redacted", "$os_version": "redacted", "$os_distro": "redacted", "$go_version": "redacted"}))
			require.NoError(t, reporter.Close())
			require.Len(t, bodies, len(tc.paths))
			for _, path := range tc.paths {
				received := <-bodies
				assert.Equal(t, path, received.path)
				var envelope struct {
					APIKey string                       `json:"api_key"`
					Batch  []map[string]json.RawMessage `json:"batch"`
				}
				require.NoError(t, json.Unmarshal(received.body, &envelope))
				assert.Equal(t, "phc_test", envelope.APIKey)
				require.Len(t, envelope.Batch, 1)
				event := envelope.Batch[0]
				assert.ElementsMatch(t, []string{"event", "distinct_id", "uuid", "timestamp", "properties"}, slices.Collect(maps.Keys(event)))
				assert.Equal(t, `"screen_view"`, string(event["event"]))
				assert.Equal(t, `"synthetic-install"`, string(event["distinct_id"]))
				assert.Equal(t, `"2026-01-02T03:04:05Z"`, string(event["timestamp"]))
				assert.NotEmpty(t, event["uuid"])
				var properties map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(event["properties"], &properties))
				assert.ElementsMatch(t, []string{"application", "source", "version", "commit", "distribution", "goos", "goarch", "install_age_hours", "$lib", "$lib_version", "$process_person_profile", "$geoip_disable", "screen", "count"}, slices.Collect(maps.Keys(properties)))
				for key, want := range map[string]string{
					"application": `"test"`, "source": `"cli"`, "version": `"v1.2.3"`, "commit": `"abc123"`, "distribution": `"homebrew"`,
					"goos": `"` + runtime.GOOS + `"`, "goarch": `"` + runtime.GOARCH + `"`,
					"install_age_hours": "48", "$lib": `"posthog-go"`, "$process_person_profile": "false", "$geoip_disable": "true",
					"screen": `"home"`, "count": "9007199254740993",
				} {
					assert.Equal(t, want, string(properties[key]), key)
				}
				assert.NotEmpty(t, properties["$lib_version"])
			}
		})
	}
}

func TestPostHogReporterCloseAfterProcessDisableDoesNotFlushQueuedEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		deliveries:    &deliveryCallbacks{},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": 1}))
	require.NotNil(client.message)

	DisableProcess()
	t.Cleanup(enablePostHogTelemetryForTest)

	require.NoError(reporter.Close())
	assert.True(client.closed)
	assert.False(reporter.Enabled())
}

func TestPostHogReporterCloseWaitsForInFlightCapture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &blockingPostHogClient{
		enqueueStarted: make(chan struct{}),
		unblockEnqueue: make(chan struct{}),
	}
	reporter := &Reporter{
		client:        client,
		deliveries:    &deliveryCallbacks{},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	captureErr := make(chan error, 1)
	go func() {
		captureErr <- reporter.Capture("daemon_active", map[string]any{"project_count": 1})
	}()
	<-client.enqueueStarted

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- reporter.Close()
	}()

	assert.Never(client.closeCalled.Load, 50*time.Millisecond, 5*time.Millisecond)

	close(client.unblockEnqueue)
	require.NoError(<-captureErr)
	require.NoError(<-closeErr)
	assert.True(client.closeCalled.Load())
	assert.False(reporter.Enabled())
}

func TestAllowTelemetryStringValues(t *testing.T) {
	filter := AllowStringValues("pulls.list")

	value, ok := filter(" pulls.list ")
	require.True(t, ok)
	assert.Equal(t, "pulls.list", value)

	_, ok = filter("private-project-name")
	assert.False(t, ok)
}

func testAllowedTelemetryOptions() []Option {
	return []Option{
		WithAllowedEvent("daemon_active",
			AllowProperty("project_count", AllowNumber),
			AllowProperty("sync_enabled", AllowBool),
			AllowProperty("view", AllowStringValues("dashboard", "summary")),
		),
		WithAllowedEvent("daemon_started",
			AllowProperty("project_count", AllowNumber),
			AllowProperty("sync_enabled", AllowBool),
		),
	}
}

func testAllowedTelemetryEvents() map[string]map[string]PropertyFilter {
	return map[string]map[string]PropertyFilter{
		"daemon_active": {
			"project_count": AllowNumber,
			"sync_enabled":  AllowBool,
			"view":          AllowStringValues("dashboard", "summary"),
		},
		"daemon_started": {
			"project_count": AllowNumber,
			"sync_enabled":  AllowBool,
		},
	}
}

func enablePostHogTelemetryForTest() {
	postHogTelemetryDisabled.Store(false)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReporterRoutesSDKLogsToSlog(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic ingest rejection", http.StatusUnauthorized)
	}))
	defer server.Close()
	reporter, err := NewReporter(Options{
		APIKey: "phc_test", Endpoint: server.URL, Application: "test",
		EnvPrefix: "TEST", DistinctID: "synthetic-install", Version: "v1.2.3",
	}, WithAllowedEvent(EventDaemonActive))
	require.NoError(t, err)
	require.NoError(t, reporter.Capture(EventDaemonActive, nil))
	_ = reporter.Close()
	assert.Contains(t, logs.String(), `"level":"INFO"`)
	assert.Contains(t, logs.String(), `"component":"posthog"`)
	assert.Contains(t, logs.String(), "synthetic ingest rejection")
}

func TestReporterPreservesSDKLogLevelsWithCustomLogger(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reporter, err := newPostHogReporter(Options{
		APIKey: "phc_test", Application: "test", EnvPrefix: "TEST", DistinctID: "synthetic-install", Version: "v1.2.3", Logger: logger,
	}, func(_ string, config phsdk.Config) (postHogEnqueueCloser, error) {
		require.NotNil(t, config.Logger)
		config.Logger.Debugf("debug %d", 1)
		config.Logger.Logf("info %d", 2)
		config.Logger.Warnf("warning %d", 3)
		config.Logger.Errorf("error %d", 4)
		return &fakePostHogClient{}, nil
	}, WithAllowedEvent(EventDaemonActive))
	require.NoError(t, err)
	require.NoError(t, reporter.Close())
	for _, pair := range [][2]string{{"DEBUG", "debug 1"}, {"INFO", "info 2"}, {"WARN", "warning 3"}, {"ERROR", "error 4"}} {
		assert.Contains(t, logs.String(), `"level":"`+pair[0]+`","msg":"`+pair[1]+`","component":"posthog"`)
	}
}

type countingLogStringer struct{ calls int }

func (s *countingLogStringer) String() string { s.calls++; return "queued event" }

func TestSDKLoggerFormatsOnlyEnabledDebug(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var logs bytes.Buffer
			value := &countingLogStringer{}
			logger := sdkLogger{slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: level}))}
			logger.Debugf("%s", value)
			if level == slog.LevelInfo {
				assert.Zero(t, value.calls)
				assert.Empty(t, logs.String())
			} else {
				assert.Equal(t, 1, value.calls)
			}
		})
	}
}
