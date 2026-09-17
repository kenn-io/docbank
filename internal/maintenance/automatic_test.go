package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
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

func TestRunPackScheduleReleasesAStalledRunAndRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 200 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		firstStarted := make(chan struct{})
		firstReleased := make(chan struct{})
		secondStarted := make(chan struct{})
		var calls atomic.Int32
		var logOutput bytes.Buffer
		done := make(chan error, 1)
		schedulerDone := make(chan struct{})
		t.Cleanup(func() {
			cancel()
			<-schedulerDone
		})
		go func() {
			defer close(schedulerDone)
			done <- RunPackSchedule(ctx, interval, func(runCtx context.Context) (PackReport, error) {
				switch calls.Add(1) {
				case 1:
					close(firstStarted)
					<-runCtx.Done()
					close(firstReleased)
					return PackReport{Stats: packstore.PackStats{
						BlobsPacked: 2, BytesPacked: 123, PacksSealed: 1,
					}}, errors.Join(context.Canceled, runCtx.Err())
				case 2:
					close(secondStarted)
					cancel()
				}
				return PackReport{}, nil
			}, slog.New(slog.NewTextHandler(&logOutput, nil)))
		}()

		<-firstStarted
		time.Sleep(interval)
		synctest.Wait()
		select {
		case <-firstReleased:
		default:
			t.Fatal("stalled pack was not released at its interval")
		}

		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
			t.Fatal("retry started before a full interval")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
		default:
			t.Fatal("retry did not start after a full interval")
		}
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, int32(2), calls.Load())
		assert.Contains(t, logOutput.String(), `msg="automatic packing canceled at interval; retrying"`)
		assert.Contains(t, logOutput.String(), "blobs=2")
		assert.Contains(t, logOutput.String(), "raw_bytes=123")
		assert.Contains(t, logOutput.String(), "packs=1")
		assert.NotContains(t, logOutput.String(), "more=")
	})
}

func TestRunPackSchedulePreservesFastRunTimingAndLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 200 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		var logOutput bytes.Buffer
		firstStarted := make(chan struct{})
		secondStarted := make(chan struct{})
		var calls atomic.Int32
		done := make(chan error, 1)
		schedulerDone := make(chan struct{})
		t.Cleanup(func() {
			cancel()
			<-schedulerDone
		})
		go func() {
			defer close(schedulerDone)
			done <- RunPackSchedule(ctx, interval, func(context.Context) (PackReport, error) {
				switch calls.Add(1) {
				case 1:
					close(firstStarted)
					return PackReport{More: true}, nil
				case 2:
					close(secondStarted)
					cancel()
				}
				return PackReport{}, nil
			}, slog.New(slog.NewTextHandler(&logOutput, nil)))
		}()

		<-firstStarted
		synctest.Wait()
		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
			t.Fatal("fast pack ran before the configured interval")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
		default:
			t.Fatal("fast pack did not run at the configured interval")
		}
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Contains(t, logOutput.String(), `msg="automatic packing completed"`)
		assert.NotContains(t, logOutput.String(), "canceled at interval")
	})
}

func TestRunPackSchedulePreservesJoinedRunFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 200 * time.Millisecond
		sentinel := errors.New("pack failed independently")
		var calls atomic.Int32
		done := make(chan error, 1)
		go func() {
			done <- RunPackSchedule(t.Context(), interval,
				func(runCtx context.Context) (PackReport, error) {
					calls.Add(1)
					<-runCtx.Done()
					return PackReport{}, errors.Join(runCtx.Err(), sentinel)
				}, nil)
		}()

		time.Sleep(interval)
		synctest.Wait()
		err := <-done
		require.ErrorIs(t, err, sentinel)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "automatic packing")
		assert.Equal(t, int32(1), calls.Load())
	})
}

func TestRunPackScheduleRetriesTransactionCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 200 * time.Millisecond
		var calls atomic.Int32
		secondStarted := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- RunPackSchedule(t.Context(), interval,
				func(runCtx context.Context) (PackReport, error) {
					switch calls.Add(1) {
					case 1:
						<-runCtx.Done()
						return PackReport{}, errors.Join(runCtx.Err(), sql.ErrTxDone)
					case 2:
						close(secondStarted)
						return PackReport{}, context.Canceled
					default:
						return PackReport{}, nil
					}
				}, nil)
		}()

		time.Sleep(interval)
		synctest.Wait()
		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
			t.Fatal("transaction cancellation retry started before a full interval")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
		default:
			t.Fatal("transaction cancellation was not retried")
		}
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, int32(2), calls.Load())
	})
}

func TestRunPackScheduleWaitsAfterSlowRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 200 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondStarted := make(chan struct{})
		var calls atomic.Int32
		done := make(chan error, 1)
		schedulerDone := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(releaseFirst) })
			cancel()
			<-schedulerDone
		})
		go func() {
			defer close(schedulerDone)
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
		time.Sleep(interval + 50*time.Millisecond)
		releaseOnce.Do(func() { close(releaseFirst) })
		synctest.Wait()
		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
			t.Fatal("second pack started without a full interval after the slow run")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-secondStarted:
		default:
			t.Fatal("second pack did not start after the post-run interval")
		}
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestRunPackScheduleRejectsInvalidConfiguration(t *testing.T) {
	run := func(context.Context) (PackReport, error) { return PackReport{}, nil }
	require.ErrorContains(t, RunPackSchedule(t.Context(), 0, run, nil), "interval")
	require.ErrorContains(t, RunPackSchedule(t.Context(), time.Second, nil, nil), "runner")
}
