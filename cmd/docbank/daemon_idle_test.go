package main

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestIdleWatchStopsDaemonAfterQuietTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := api.NewActivityTracker()
		var stops atomic.Int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			idleWatch(t.Context(), tracker, 30*time.Minute, slog.New(slog.DiscardHandler), func() { stops.Add(1) })
		}()
		time.Sleep(30*time.Minute - time.Nanosecond)
		synctest.Wait()
		assert.Zero(t, stops.Load(), "a daemon quiet for less than idle_timeout keeps running")
		time.Sleep(time.Nanosecond)
		<-done
		assert.Equal(t, int64(1), stops.Load())
	})
}

func TestIdleWatchWaitsWhileActivityIsHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := api.NewActivityTracker()
		tracker.Begin()
		var stops atomic.Int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			idleWatch(t.Context(), tracker, 30*time.Minute, slog.New(slog.DiscardHandler), func() { stops.Add(1) })
		}()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		assert.Zero(t, stops.Load(), "held activity, such as a backup schedule, keeps the daemon running")

		tracker.End()
		time.Sleep(30*time.Minute - time.Nanosecond)
		synctest.Wait()
		assert.Zero(t, stops.Load(), "the quiet window starts when the activity ends")
		time.Sleep(time.Nanosecond)
		<-done
		require.Equal(t, int64(1), stops.Load())
	})
}
