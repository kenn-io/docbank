package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunPackScheduleRunsImmediatelyAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	err := RunPackSchedule(ctx, time.Hour, func(context.Context) (PackReport, error) {
		calls.Add(1)
		cancel()
		return PackReport{}, nil
	}, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), calls.Load())
}

func TestRunPackScheduleReportsRunFailure(t *testing.T) {
	sentinel := errors.New("pack failed")
	err := RunPackSchedule(t.Context(), time.Hour,
		func(context.Context) (PackReport, error) { return PackReport{}, sentinel }, nil)
	require.ErrorIs(t, err, sentinel)
	require.ErrorContains(t, err, "automatic packing")
}

func TestRunPackScheduleWaitsAfterSlowRun(t *testing.T) {
	const interval = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- RunPackSchedule(ctx, interval, func(context.Context) (PackReport, error) {
			switch calls.Add(1) {
			case 1:
				close(firstStarted)
				<-releaseFirst
			case 2:
				close(secondStarted)
				cancel()
			}
			return PackReport{}, nil
		}, slog.New(slog.DiscardHandler))
	}()

	<-firstStarted
	// Let the old ticker-based implementation queue a tick while the first
	// run is still active, then prove completion begins a fresh quiet window.
	time.Sleep(interval + 50*time.Millisecond)
	close(releaseFirst)
	select {
	case <-secondStarted:
		t.Fatal("second pack started without a full interval after the slow run")
	case <-time.After(interval / 2):
	}
	select {
	case <-secondStarted:
	case <-time.After(2 * interval):
		t.Fatal("second pack did not start after the post-run interval")
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("pack schedule did not stop after cancellation")
	}
}

func TestRunPackScheduleRejectsInvalidConfiguration(t *testing.T) {
	run := func(context.Context) (PackReport, error) { return PackReport{}, nil }
	require.ErrorContains(t, RunPackSchedule(t.Context(), 0, run, nil), "interval")
	require.ErrorContains(t, RunPackSchedule(t.Context(), time.Second, nil, nil), "runner")
}
