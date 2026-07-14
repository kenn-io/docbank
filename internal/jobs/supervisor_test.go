package jobs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupervisorTracksDeterministicTerminalStates(t *testing.T) {
	s := New(t.Context(), discardLogger())
	blocked := make(chan struct{})
	require.NoError(t, s.Start("z-running", func(ctx context.Context) error {
		close(blocked)
		<-ctx.Done()
		return ctx.Err()
	}))
	<-blocked
	require.NoError(t, s.Start("a-complete", func(context.Context) error { return nil }))
	require.NoError(t, s.Start("m-failed", func(context.Context) error { return errors.New("broken") }))

	require.Eventually(t, func() bool {
		jobs := s.Snapshot()
		return len(jobs) == 3 && jobs[0].Status == StatusCompleted && jobs[1].Status == StatusFailed
	}, time.Second, time.Millisecond)
	jobs := s.Snapshot()
	assert.Equal(t, []string{"a-complete", "m-failed", "z-running"},
		[]string{jobs[0].Name, jobs[1].Name, jobs[2].Name})
	assert.Equal(t, "broken", jobs[1].Error)
	assert.Nil(t, jobs[2].FinishedAt)

	require.NoError(t, s.Shutdown(t.Context()))
	jobs = s.Snapshot()
	assert.Equal(t, StatusCancelled, jobs[2].Status)
	assert.NotNil(t, jobs[2].FinishedAt)
}

func TestSupervisorRejectsDuplicateInvalidAndPostStopJobs(t *testing.T) {
	s := New(t.Context(), discardLogger())
	require.ErrorIs(t, s.Start("Bad Name", func(context.Context) error { return nil }), ErrInvalid)
	require.ErrorIs(t, s.Start("missing", nil), ErrInvalid)
	require.NoError(t, s.Start("watch:inbox", func(context.Context) error { return nil }))
	require.ErrorIs(t, s.Start("watch:inbox", func(context.Context) error { return nil }), ErrDuplicate)
	require.NoError(t, s.Shutdown(t.Context()))
	require.ErrorIs(t, s.Start("late", func(context.Context) error { return nil }), ErrClosed)
}

func TestSupervisorCapturesPanicsAndBoundsErrors(t *testing.T) {
	s := New(t.Context(), discardLogger())
	require.NoError(t, s.Start("panic", func(context.Context) error { panic("boom") }))
	require.NoError(t, s.Start("long-error", func(context.Context) error {
		return errors.New(strings.Repeat("x", 5000))
	}))
	require.Eventually(t, func() bool {
		jobs := s.Snapshot()
		return len(jobs) == 2 && jobs[0].Status == StatusFailed && jobs[1].Status == StatusFailed
	}, time.Second, time.Millisecond)
	jobs := s.Snapshot()
	assert.Equal(t, "long-error", jobs[0].Name)
	assert.Len(t, jobs[0].Error, 4096)
	assert.Equal(t, "panic: boom", jobs[1].Error)
	require.NoError(t, s.Shutdown(t.Context()))
}

func TestSupervisorShutdownHonorsDeadline(t *testing.T) {
	s := New(t.Context(), discardLogger())
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	require.NoError(t, s.Start("stuck", func(context.Context) error {
		<-release
		return nil
	}))

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Shutdown(ctx), context.DeadlineExceeded)
	close(release)
	released = true
	require.NoError(t, s.Shutdown(t.Context()))
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
