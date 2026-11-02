package posthog

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingPostHogClient struct {
	mu       sync.Mutex
	messages []phsdk.Capture
	closes   int
}

func (c *recordingPostHogClient) Enqueue(message phsdk.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, message.(phsdk.Capture))
	return nil
}

func (*recordingPostHogClient) FlushWithContext(context.Context) error { return nil }

func (c *recordingPostHogClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}

type fakePostHogClock struct {
	now time.Time
}

func (c *fakePostHogClock) Now() time.Time { return c.now }

func (c *fakePostHogClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

var postHogTestStart = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newInstallAgeTestReporter(t *testing.T, installedAt time.Time, options ...Option) (*Reporter, *recordingPostHogClient, *fakePostHogClock) {
	t.Helper()
	client := &recordingPostHogClient{}
	reporter, clock := newInstallAgeTestReporterWithClient(t, client, installedAt, options...)
	return reporter, client, clock
}

func newInstallAgeTestReporterWithClient(t *testing.T, client postHogEnqueueCloser, installedAt time.Time, options ...Option) (*Reporter, *fakePostHogClock) {
	t.Helper()
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	clock := &fakePostHogClock{now: postHogTestStart}
	allOptions := append(testAllowedTelemetryOptions(), postHogOptionFunc(func(config *postHogReporterConfig) {
		config.now = clock.Now
	}))
	allOptions = append(allOptions, options...)
	reporter, err := newPostHogReporter(Options{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		Version:     "v1.2.3",
		InstalledAt: installedAt,
	}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
		return client, nil
	}, allOptions...)
	require.NoError(t, err)
	return reporter, clock
}

func capturedEvents(messages []phsdk.Capture) []string {
	events := make([]string, 0, len(messages))
	for _, message := range messages {
		events = append(events, message.Event)
	}
	return events
}

func allowAnyTelemetryValue(value any) (any, bool) { return value, true }

func TestPostHogReporterTagsInstallAge(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*60*60)
	tests := []struct {
		name        string
		installedAt time.Time
		want        int64
	}{
		{name: "23h", installedAt: postHogTestStart.Add(-23 * time.Hour), want: 23},
		{name: "25h", installedAt: postHogTestStart.Add(-25 * time.Hour), want: 25},
		{name: "24h_minus_1s", installedAt: postHogTestStart.Add(-(24*time.Hour - time.Second)), want: 23},
		{name: "24h", installedAt: postHogTestStart.Add(-24 * time.Hour), want: 24},
		{name: "future_2h", installedAt: postHogTestStart.Add(2 * time.Hour), want: 0},
		{name: "non_utc_23h", installedAt: postHogTestStart.Add(-23 * time.Hour).In(zone), want: 23},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			reporter, client, clock := newInstallAgeTestReporter(t, tt.installedAt)

			require.NoError(reporter.Capture("daemon_active", nil))

			require.Len(client.messages, 1)
			assert.Equal(tt.want, client.messages[0].Properties["install_age_hours"])
			assert.IsType(int64(0), client.messages[0].Properties["install_age_hours"])
			assert.Equal(clock.Now().UTC(), client.messages[0].Timestamp)
			assert.Equal(time.UTC, client.messages[0].Timestamp.Location())
		})
	}
}

func TestPostHogReporterInstallAgeGrowsPerCapture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock := newInstallAgeTestReporter(t, postHogTestStart.Add(2*time.Hour))

	require.NoError(reporter.Capture("daemon_started", nil))
	require.Len(client.messages, 1)
	clock.Advance(3 * time.Hour)
	require.NoError(reporter.Capture("daemon_active", nil))
	require.Len(client.messages, 2)
	clock.Advance(30 * time.Hour)
	require.NoError(reporter.Capture("daemon_active", nil))
	require.Len(client.messages, 3)

	assert.Equal([]string{"daemon_started", "daemon_active", "daemon_active"}, capturedEvents(client.messages))
	ages := make([]any, 0, len(client.messages))
	for _, message := range client.messages {
		ages = append(ages, message.Properties["install_age_hours"])
	}
	assert.Equal([]any{int64(0), int64(1), int64(31)}, ages)
}

func TestPostHogReporterZeroInstalledAtOmitsInstallAge(t *testing.T) {
	require := require.New(t)

	reporter, client, _ := newInstallAgeTestReporter(t, time.Time{})

	require.NoError(reporter.Capture("daemon_started", map[string]any{"sync_enabled": true}))

	require.Len(client.messages, 1)
	assert.Equal(t, phsdk.Properties{
		"$process_person_profile": false,
		"$geoip_disable":          true,
		"application":             "kata",
		"source":                  "daemon",
		"version":                 "v1.2.3",
		"commit":                  "",
		"distribution":            "",
		"goos":                    runtime.GOOS,
		"goarch":                  runtime.GOARCH,
		"sync_enabled":            true,
	}, client.messages[0].Properties)
}

func TestPostHogReporterOwnsInstallAgeProperty(t *testing.T) {
	tests := []struct {
		name        string
		installedAt time.Time
		want        any
	}{
		{name: "25h", installedAt: postHogTestStart.Add(-25 * time.Hour), want: int64(25)},
		{name: "zero", installedAt: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			reporter, client, _ := newInstallAgeTestReporter(t, tt.installedAt,
				WithAllowedEvent("daemon_active", AllowProperty("install_age_hours", allowAnyTelemetryValue)))

			require.NoError(reporter.Capture("daemon_active", map[string]any{
				"install_age_hours":   9999,
				" install_age_hours ": 9999, //nolint:gocritic // padded key checks that trimmed caller keys cannot set the age
				"path":                "/Users/example/private",
				"project_count":       math.Inf(1),
			}))

			require.Len(client.messages, 1)
			props := client.messages[0].Properties
			if tt.want == nil {
				assert.NotContains(props, "install_age_hours")
			} else {
				assert.Equal(tt.want, props["install_age_hours"])
			}
			assert.NotContains(props, "path")
			assert.NotContains(props, "project_count")
		})
	}
}

func TestPostHogReporterSanitizePropertiesOmitsInstallAge(t *testing.T) {
	require := require.New(t)

	reporter, _, _ := newInstallAgeTestReporter(t, postHogTestStart.Add(-25*time.Hour),
		WithAllowedEvent("daemon_active", AllowProperty("install_age_hours", allowAnyTelemetryValue)))

	props, err := reporter.SanitizeProperties("daemon_active", map[string]any{
		"install_age_hours": 9999,
		"project_count":     1,
	})

	require.NoError(err)
	assert.NotContains(t, props, "install_age_hours")
	assert.Equal(t, 1, props["project_count"])
}

func TestPostHogReporterYoungInstallSendsEveryCapture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock := newInstallAgeTestReporter(t, postHogTestStart)

	for i := range 300 {
		require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": i}))
		require.Len(client.messages, i+1)
		clock.Advance(time.Second)
	}
	for i, message := range client.messages {
		assert.Equal(i, message.Properties["project_count"])
		assert.Equal(postHogTestStart.Add(time.Duration(i)*time.Second), message.Timestamp)
	}

	require.NoError(reporter.Close())
	require.NoError(reporter.Close())
	assert.Len(client.messages, 300)
	assert.Equal(1, client.closes)
	assert.False(reporter.Enabled())
}

func TestPostHogReporterInstalledAtHonorsOptOut(t *testing.T) {
	envTests := []struct {
		name     string
		generic  string
		prefixed string
	}{
		{name: "generic", generic: " 0 ", prefixed: "1"},
		{name: "prefixed", generic: "1", prefixed: "0"},
	}
	for _, tt := range envTests {
		t.Run(tt.name, func(t *testing.T) {
			enablePostHogTelemetryForTest()
			t.Cleanup(enablePostHogTelemetryForTest)
			t.Setenv(GenericEnabledEnv, tt.generic)
			t.Setenv("KATA_TELEMETRY_ENABLED", tt.prefixed)

			reporter, err := newPostHogReporter(Options{
				APIKey:      "caller-owned-key",
				Application: "kata",
				EnvPrefix:   "KATA",
				DistinctID:  "anonymous-instance-id",
				Version:     "v1.2.3",
				InstalledAt: postHogTestStart.Add(-7 * time.Minute),
			}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
				assert.Fail(t, "client factory called despite opt-out")
				return nil, errors.New("client factory called")
			}, testAllowedTelemetryOptions()...)

			require.NoError(t, err)
			assert.False(t, reporter.Enabled())
			require.NoError(t, reporter.Capture("daemon_active", nil))
		})
	}

	t.Run("process_disable", func(t *testing.T) {
		reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart.Add(-61*time.Minute))

		DisableProcess()

		require.NoError(t, reporter.Capture("daemon_active", map[string]any{"project_count": 1}))
		assert.Empty(t, client.messages)
		assert.False(t, reporter.Enabled())
	})
}

type failingPostHogClient struct {
	enqueueErr error
	flushErr   error
	closeErr   error
}

func (c failingPostHogClient) Enqueue(phsdk.Message) error { return c.enqueueErr }

func (c failingPostHogClient) FlushWithContext(context.Context) error { return c.flushErr }

func (c failingPostHogClient) Close() error { return c.closeErr }

func TestPostHogReporterReturnsClientErrorsUnwrapped(t *testing.T) {
	enqueueErr := errors.New("enqueue failed")
	closeErr := errors.New("close failed")
	reporter, _ := newInstallAgeTestReporterWithClient(t,
		failingPostHogClient{enqueueErr: enqueueErr, closeErr: closeErr}, postHogTestStart)

	assert.Equal(t, enqueueErr, reporter.Capture("daemon_active", nil))
	assert.Equal(t, closeErr, reporter.Close())
	assert.False(t, reporter.Enabled())
	assert.NoError(t, reporter.Close())
}
