package posthog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type heartbeatClient struct {
	mu       sync.Mutex
	events   []string
	err      error
	disabled atomic.Bool
}

func (c *heartbeatClient) Capture(event string, _ map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return c.err
}

func (c *heartbeatClient) Close() error  { return nil }
func (c *heartbeatClient) Enabled() bool { return !c.disabled.Load() }

func (c *heartbeatClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func TestRunHeartbeatHonorsReporterOptOut(t *testing.T) {
	for _, optOut := range []bool{false, true} {
		t.Run(strconv.FormatBool(optOut), func(t *testing.T) {
			if optOut {
				t.Setenv(GenericEnabledEnv, "0")
			}
			synctest.Test(t, func(t *testing.T) {
				client := &recordingPostHogClient{}
				reporter, err := newPostHogReporter(Options{
					APIKey: "caller-owned-key", Application: "kata", EnvPrefix: "KATA",
					DistinctID: "anonymous-instance-id", Version: "v1.2.3",
				}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
					return client, nil
				}, WithAllowedEvent(EventDaemonActive))
				require.NoError(t, err)
				defer func() { require.NoError(t, reporter.Close()) }()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go RunHeartbeat(ctx, reporter, nil)
				synctest.Wait()
				if !optOut {
					require.Len(t, client.messages, 1)
					assert.Equal(t, EventDaemonActive, client.messages[0].Event)
				} else {
					assert.Empty(t, client.messages)
				}
				cancel()
				synctest.Wait()
			})
		})
	}
}

func TestRunHeartbeatStopsWhenDisabled(t *testing.T) {
	for name, initiallyDisabled := range map[string]bool{"initially disabled": true, "disabled after first send": false} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &heartbeatClient{}
				client.disabled.Store(initiallyDisabled)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				go func() { RunHeartbeat(ctx, client, nil); close(done) }()
				synctest.Wait()
				want := 0
				if !initiallyDisabled {
					want = 1
					client.disabled.Store(true)
					time.Sleep(time.Hour)
					synctest.Wait()
				}
				select {
				case <-done:
				default:
					assert.Fail(t, "disabled heartbeat must return while context is active")
				}
				assert.Equal(t, want, client.count())
			})
		})
	}
}

func TestRunHeartbeatFollowsUTCDays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Start half an hour before midnight, not at the beginning of a UTC day.
		time.Sleep(23*time.Hour + 30*time.Minute)
		client := &heartbeatClient{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go RunHeartbeat(ctx, client, nil)
		synctest.Wait()
		assert.Equal(t, 1, client.count())
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal(t, 2, client.count(), "the UTC date changed after only one hour")
		time.Sleep(22 * time.Hour)
		synctest.Wait()
		assert.Equal(t, 2, client.count(), "the same UTC day must not send again")
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		assert.Equal(t, 3, client.count())
	})
}

func TestRunHeartbeatLogsCaptureErrorsAndKeepsRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &heartbeatClient{err: errors.New("queue full")}
		var logs bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		go RunHeartbeat(ctx, client, slog.New(slog.NewTextHandler(&logs, nil)))
		synctest.Wait()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		cancel()
		synctest.Wait()
		assert.Equal(t, 2, client.count())
		assert.Contains(t, logs.String(), "queue full")
	})
}

func TestRunHeartbeatSendsNothingOnceCanceled(t *testing.T) {
	client := &heartbeatClient{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	RunHeartbeat(ctx, client, nil)

	assert.Zero(t, client.count())
}

func TestRunHeartbeatUsesCurrentWallDateAfterSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &heartbeatClient{}
		now := time.Date(2026, time.October, 3, 23, 30, 0, 0, time.UTC)
		staleTick := now
		ticks := make(chan time.Time)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go runHeartbeat(ctx, client, ticks, func() time.Time { return now }, nil)
		synctest.Wait()
		assert.Equal(t, 1, client.count())
		for _, tc := range []struct {
			date string
			want int
		}{
			{"2026-10-03T23:45:00Z", 1},
			{"2026-10-04T01:00:00+01:00", 2},
			{"2026-10-07T08:00:00Z", 3}, // Several sleeping days produce one current event.
			{"2026-10-07T09:00:00Z", 3},
			{"2026-10-06T09:00:00Z", 3}, // A clock correction must not repeat an older day.
		} {
			var err error
			now, err = time.Parse(time.RFC3339, tc.date)
			require.NoError(t, err)
			ticks <- staleTick
			synctest.Wait()
			assert.Equal(t, tc.want, client.count(), tc.date)
		}
	})
}
