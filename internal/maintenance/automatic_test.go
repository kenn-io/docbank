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

func TestRunPackScheduleRejectsInvalidConfiguration(t *testing.T) {
	run := func(context.Context) (PackReport, error) { return PackReport{}, nil }
	require.ErrorContains(t, RunPackSchedule(t.Context(), 0, run, nil), "interval")
	require.ErrorContains(t, RunPackSchedule(t.Context(), time.Second, nil, nil), "runner")
}
