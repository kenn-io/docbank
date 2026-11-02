package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleWaitsForItsDelayOutsideAWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		require.NoError(t, Idle(t.Context(), time.Minute, false))
		assert.Equal(t, time.Minute, time.Since(start))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, Idle(ctx, time.Minute, false), context.Canceled)
	})
}

func TestIdleKeepsWorkerIntervalWithoutSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(t.Context(), discardLogger())
		var polls atomic.Int64
		require.NoError(t, s.StartWorker("poller", func(ctx context.Context) error {
			for {
				polls.Add(1)
				if err := Idle(ctx, time.Hour, false); err != nil {
					return err
				}
			}
		}))
		synctest.Wait()
		require.Equal(t, int64(1), polls.Load())
		time.Sleep(time.Hour - time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int64(1), polls.Load(), "no early poll without Settle")
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int64(2), polls.Load())
		require.NoError(t, s.Shutdown(t.Context()))
	})
}

func TestSettleFinishesWorkHandedBetweenWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// queueWorker polls its inbox and forwards every item to next, if set.
		// It takes a second to forward, so next has already polled and found
		// nothing.
		queueWorker := func(inbox <-chan int, next chan<- int, handled *atomic.Int64) func(context.Context) error {
			return func(ctx context.Context) error {
				for {
					found := false
				drain:
					for {
						select {
						case item := <-inbox:
							found = true
							handled.Add(1)
							if next != nil {
								time.Sleep(time.Second)
								next <- item
							}
						default:
							break drain
						}
					}
					if err := Idle(ctx, time.Hour, found); err != nil {
						return err
					}
				}
			}
		}
		s := New(t.Context(), discardLogger())
		first, second := make(chan int, 10), make(chan int, 10)
		var firstHandled, secondHandled atomic.Int64
		require.NoError(t, s.StartWorker("second", queueWorker(second, nil, &secondHandled)))
		require.NoError(t, s.StartWorker("first", queueWorker(first, second, &firstHandled)))
		require.NoError(t, s.Start("unrelated", func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}))
		synctest.Wait()
		first <- 1
		first <- 2

		start := time.Now()
		require.NoError(t, s.Settle(t.Context()))
		assert.Less(t, time.Since(start), time.Hour, "Settle wakes parked workers instead of waiting their interval")
		assert.Equal(t, int64(2), firstHandled.Load())
		assert.Equal(t, int64(2), secondHandled.Load(), "work handed to another worker settles too")
		require.NoError(t, s.Shutdown(t.Context()))
	})
}

func TestSettleRepollsAWorkerWhosePollBeganBeforeTheRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(t.Context(), discardLogger())
		var queued, handled atomic.Int64
		polling, release := make(chan struct{}), make(chan struct{})
		first := true
		require.NoError(t, s.StartWorker("worker", func(ctx context.Context) error {
			for {
				// The poll reads the queue before blocking, so on the first
				// pass it misses work queued while it is blocked.
				pending := queued.Load() - handled.Load()
				if first {
					first = false
					close(polling)
					<-release
				}
				handled.Add(pending)
				if err := Idle(ctx, time.Hour, pending > 0); err != nil {
					return err
				}
			}
		}))
		<-polling
		queued.Add(1)
		settled := make(chan error, 1)
		go func() { settled <- s.Settle(t.Context()) }()
		synctest.Wait()
		select {
		case err := <-settled:
			require.Failf(t, "Settle returned while a worker was polling", "%v", err)
		default:
		}
		close(release)
		require.NoError(t, <-settled)
		assert.Equal(t, int64(1), handled.Load(), "Settle must re-poll a worker whose poll began before it")
		require.NoError(t, s.Shutdown(t.Context()))
	})
}

func TestSettleReportsWorkersStillPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(t.Context(), discardLogger())
		stuck := make(chan struct{})
		require.NoError(t, s.StartWorker("stuck", func(ctx context.Context) error {
			select {
			case <-stuck:
			case <-ctx.Done():
			}
			return nil
		}))
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		err := s.Settle(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "still polling: stuck")
		close(stuck)
		synctest.Wait()
		require.NoError(t, s.Settle(t.Context()), "a finished worker no longer holds Settle")
		require.NoError(t, s.Shutdown(t.Context()))
	})
}

func TestSettleWakesAWorkerParkedInItsOwnSelect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(t.Context(), discardLogger())
		var queued, handled atomic.Int64
		other := make(chan struct{})
		require.NoError(t, s.StartWorker("custom", func(ctx context.Context) error {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			found := false
			for {
				settle, resume := Park(ctx, found)
				select {
				case <-ctx.Done():
					resume()
					return ctx.Err()
				case <-other:
					// Not a poll: park again without resuming.
					continue
				case <-ticker.C:
				case <-settle:
				}
				resume()
				pending := queued.Load() - handled.Load()
				handled.Add(pending)
				found = pending > 0
			}
		}))
		synctest.Wait()
		queued.Add(1)
		other <- struct{}{}
		require.NoError(t, s.Settle(t.Context()))
		assert.Equal(t, int64(1), handled.Load())
		require.NoError(t, s.Shutdown(t.Context()))
	})
}

func TestParkOutsideAWorkerNeverWakes(t *testing.T) {
	settle, resume := Park(t.Context(), true)
	assert.Nil(t, settle)
	resume()
}
