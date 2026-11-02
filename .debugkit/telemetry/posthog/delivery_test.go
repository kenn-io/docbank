package posthog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type confirmedResult struct {
	status Status
	err    error
}

func confirmAsync(ctx context.Context, r *Reporter, delivery Delivery) <-chan confirmedResult {
	result := make(chan confirmedResult, 1)
	go func() {
		status, err := r.CaptureConfirmed(ctx, "milestone", nil, delivery)
		result <- confirmedResult{status, err}
	}()
	return result
}

func newConfirmedTestReporter(t *testing.T, handler http.HandlerFunc, configure func(*phsdk.Config)) *Reporter {
	t.Helper()
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	r, err := newPostHogReporter(Options{APIKey: "test-key", Application: "test", EnvPrefix: "TEST", DistinctID: "test-install", Version: "v1.2.3", Endpoint: server.URL, InstalledAt: postHogTestStart.Add(-time.Hour)}, func(key string, config phsdk.Config) (postHogEnqueueCloser, error) {
		config.Compression = phsdk.CompressionNone
		configure(&config)
		return phsdk.NewWithConfig(key, config)
	}, WithAllowedEvent("milestone"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestCaptureConfirmedDeliveryOutcomes(t *testing.T) {
	for code, wantAttempts := range map[int]int{
		http.StatusOK:                 1,
		http.StatusBadRequest:         1,
		http.StatusServiceUnavailable: 2,
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var attempts atomic.Int32
			r := newConfirmedTestReporter(t, func(w http.ResponseWriter, _ *http.Request) {
				attempt := attempts.Add(1)
				if code == http.StatusServiceUnavailable && attempt > 1 {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(code)
				}
			}, func(config *phsdk.Config) {
				config.RetryAfter = func(int) time.Duration { return time.Millisecond }
			})
			status, err := r.CaptureConfirmed(t.Context(), "milestone", nil, Delivery{ID: uuid.New(), Time: postHogTestStart})
			delivered := code == http.StatusOK || code == http.StatusServiceUnavailable
			assert.Equal(t, delivered, err == nil, "delivery error: %v", err)
			assert.Equal(t, delivered, status == StatusDelivered)
			assert.Equal(t, int32(wantAttempts), attempts.Load())
		})
	}
}

func TestCaptureConfirmedAmbiguousRetryKeepsIdentity(t *testing.T) {
	received, release := make(chan phsdk.CaptureInApi, 2), make(chan struct{})
	releaseResponse := sync.OnceFunc(func() { close(release) })
	defer releaseResponse()
	handler := func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Batch []phsdk.CaptureInApi `json:"batch"`
		}
		var capture phsdk.CaptureInApi
		if assert.NoError(t, json.NewDecoder(req.Body).Decode(&body)) && assert.Len(t, body.Batch, 1) {
			capture = body.Batch[0]
		}
		received <- capture
		<-release
		w.WriteHeader(http.StatusOK)
	}
	r := newConfirmedTestReporter(t, handler, func(*phsdk.Config) {})
	delivery := Delivery{ID: uuid.New(), Time: postHogTestStart.Add(123456789 * time.Nanosecond).In(time.FixedZone("offset", -5*3600))}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := confirmAsync(ctx, r, delivery)
	first := <-received
	require.ErrorIs(t, (<-result).err, context.DeadlineExceeded)
	// A new reporter models restart with the persisted tuple and a later clock.
	retry := newConfirmedTestReporter(t, handler, func(*phsdk.Config) {})
	retry.now = func() time.Time { return postHogTestStart.Add(24 * time.Hour) }
	result = confirmAsync(t.Context(), retry, delivery)
	second := <-received
	assert.Equal(t, first, second)
	assert.InDelta(t, 1, second.Properties["install_age_hours"], 0)
	releaseResponse()
	require.NoError(t, (<-result).err)
}

// closingFlushClient starts Reporter.Close before flushing, so flush sees a
// closed SDK while the drain still has the event in flight.
type closingFlushClient struct {
	postHogEnqueueCloser
	r                 *Reporter
	inFlight, release chan struct{}
}

func (c closingFlushClient) FlushWithContext(ctx context.Context) error {
	go func() { _ = c.r.Close() }()
	<-c.inFlight
	defer close(c.release)
	return c.postHogEnqueueCloser.FlushWithContext(ctx)
}

func TestCaptureConfirmedWaitsForCloseDrain(t *testing.T) {
	inFlight, release := make(chan struct{}), make(chan struct{})
	r := newConfirmedTestReporter(t, func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		<-release
		w.WriteHeader(http.StatusOK)
	}, func(*phsdk.Config) {})
	r.client = closingFlushClient{r.client, r, inFlight, release}
	status, err := r.CaptureConfirmed(t.Context(), "milestone", nil, Delivery{ID: uuid.New(), Time: postHogTestStart})
	require.NoError(t, err)
	assert.Equal(t, StatusDelivered, status)
}

func TestCaptureConfirmedOverlappingCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, client, _ := newInstallAgeTestReporter(t, time.Time{}, WithAllowedEvent("milestone"))
		delivery := Delivery{ID: uuid.New(), Time: postHogTestStart}
		first, second := confirmAsync(t.Context(), r, delivery), confirmAsync(t.Context(), r, delivery)
		synctest.Wait()
		require.Len(t, client.messages, 2)
		capture := phsdk.CaptureInApi{Uuid: delivery.ID.String(), Timestamp: delivery.Time, Event: "milestone", DistinctId: "anonymous-instance-id"}
		for _, change := range []func(*phsdk.CaptureInApi){
			func(c *phsdk.CaptureInApi) { c.Uuid = uuid.New().String() },
			func(c *phsdk.CaptureInApi) { c.Event = "other" },
			func(c *phsdk.CaptureInApi) { c.Timestamp = c.Timestamp.Add(time.Second) },
			func(c *phsdk.CaptureInApi) { c.DistinctId = "other" },
		} {
			other := capture
			change(&other)
			r.deliveries.Success(other)
			r.deliveries.Failure(other, errors.New("unrelated"))
		}
		synctest.Wait()
		assert.Empty(t, first)
		assert.Empty(t, second)
		r.deliveries.Success(capture)
		for _, result := range []<-chan confirmedResult{first, second} {
			got := <-result
			require.NoError(t, got.err)
			assert.Equal(t, StatusDelivered, got.status)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first, second = confirmAsync(ctx, r, delivery), confirmAsync(t.Context(), r, delivery)
		synctest.Wait()
		cancel()
		require.ErrorIs(t, (<-first).err, context.Canceled)
		r.deliveries.Success(capture)
		got := <-second
		require.NoError(t, got.err)
		assert.Equal(t, StatusDelivered, got.status)
	})
}

func TestCaptureConfirmedPrecancelDoesNotEnqueue(t *testing.T) {
	r, client, _ := newInstallAgeTestReporter(t, time.Time{}, WithAllowedEvent("milestone"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := r.CaptureConfirmed(ctx, "milestone", nil, Delivery{ID: uuid.New(), Time: postHogTestStart})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, client.messages)
}

func TestCaptureConfirmedClientErrors(t *testing.T) {
	clientErr := errors.New("SDK client failed")
	for _, client := range []failingPostHogClient{{enqueueErr: clientErr}, {flushErr: clientErr}} {
		r, _ := newInstallAgeTestReporterWithClient(t, client, time.Time{}, WithAllowedEvent("milestone"))
		status, err := r.CaptureConfirmed(t.Context(), "milestone", nil, Delivery{ID: uuid.New(), Time: postHogTestStart})
		require.Same(t, clientErr, err)
		assert.Empty(t, status)
	}
}

func TestCaptureConfirmedClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _, _ := newInstallAgeTestReporter(t, time.Time{}, WithAllowedEvent("milestone"))
		result := confirmAsync(t.Context(), r, Delivery{ID: uuid.New(), Time: postHogTestStart})
		synctest.Wait()
		require.NoError(t, r.Close())
		require.ErrorIs(t, (<-result).err, phsdk.ErrClosed)
	})
}

func TestCaptureConfirmedValidationAndDisable(t *testing.T) {
	unencodable := AllowProperty("callback", func(any) (any, bool) { return func() {}, true })
	r, client, _ := newInstallAgeTestReporter(t, time.Time{}, WithAllowedEvent("milestone"),
		WithAllowedEvent("unencodable", unencodable),
		WithAllowedEvent("screen", RequireProperty("name", AllowStringValues("queue"))),
		WithDailyEvent("screen", "name", NewDailyClaims(filepath.Join(t.TempDir(), "claims.json"))))
	valid := Delivery{ID: uuid.New(), Time: postHogTestStart}
	for _, tc := range []struct {
		event    string
		delivery Delivery
	}{
		{"milestone", Delivery{Time: valid.Time}},
		{"milestone", Delivery{ID: valid.ID}},
		{"unknown", valid},
	} {
		_, err := r.CaptureConfirmed(t.Context(), tc.event, nil, tc.delivery)
		require.Error(t, err)
	}
	_, err := r.CaptureConfirmed(t.Context(), "screen", map[string]any{"name": "queue"}, valid)
	require.ErrorIs(t, err, ErrUnsupportedEvent)
	_, err = r.CaptureConfirmed(t.Context(), "unencodable", map[string]any{"callback": true}, valid)
	require.ErrorContains(t, err, "encode telemetry properties")
	assert.Empty(t, client.messages)
	t.Setenv(GenericEnabledEnv, "0")
	disabled, err := NewReporter(Options{}, WithAllowedEvent("milestone"))
	require.NoError(t, err)
	status, err := disabled.CaptureConfirmed(t.Context(), "milestone", nil, valid)
	require.NoError(t, err)
	assert.Equal(t, StatusDisabled, status)
}

func TestCaptureConfirmedProcessDisableDoesNotConfirmSyntheticSuccess(t *testing.T) {
	r := newConfirmedTestReporter(t, func(http.ResponseWriter, *http.Request) {}, func(config *phsdk.Config) {
		config.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			DisableProcess()
			return (postHogDisableTransport{}).RoundTrip(req)
		})
	})
	status, err := r.CaptureConfirmed(t.Context(), "milestone", nil, Delivery{ID: uuid.New(), Time: postHogTestStart})
	require.NoError(t, err)
	assert.Equal(t, StatusDisabled, status)
}
