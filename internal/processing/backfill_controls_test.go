package processing

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestControlledBackfillPauseAndLowerLimitWithinPage(t *testing.T) {
	t.Parallel()
	for _, pause := range []bool{true, false} {
		t.Run(strconv.FormatBool(pause), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var paused atomic.Bool
				var limit atomic.Int32
				limit.Store(3)
				started := make(chan string, 5)
				release := make(chan struct{})
				b := Backfill[string]{Key: func(s string) string { return s }, Control: func(context.Context) (store.LaneControl, error) {
					return store.LaneControl{Paused: paused.Load(), Concurrency: int(limit.Load())}, nil
				}, Process: func(_ context.Context, key string) error { started <- key; <-release; return nil }}
				done := make(chan error, 1)
				go func() {
					_, err := b.processPage(t.Context(), []string{"a", "b", "c", "d", "e"}, newBackfillRetrySet())
					done <- err
				}()
				synctest.Wait()
				require.Len(t, started, 3)
				if pause {
					paused.Store(true)
					for range 3 {
						release <- struct{}{}
					}
					synctest.Wait()
					require.Len(t, started, 3)
					limit.Store(1)
					paused.Store(false)
					time.Sleep(100 * time.Millisecond)
				} else {
					limit.Store(1)
					for range 2 {
						release <- struct{}{}
						synctest.Wait()
						require.Len(t, started, 3)
					}
					release <- struct{}{}
				}
				synctest.Wait()
				require.Len(t, started, 4)
				release <- struct{}{}
				synctest.Wait()
				require.Len(t, started, 5)
				release <- struct{}{}
				require.NoError(t, <-done)
				seen := make(map[string]bool)
				for range 5 {
					key := <-started
					require.False(t, seen[key])
					seen[key] = true
				}
			})
		})
	}
}

func TestControlledBackfillReturnsPanicsToSupervisor(t *testing.T) {
	t.Parallel()
	b := Backfill[string]{Key: func(s string) string { return s }, Control: func(context.Context) (store.LaneControl, error) {
		return store.LaneControl{Concurrency: 1}, nil
	}, Process: func(context.Context, string) error { panic("synthetic preview panic") }}
	require.PanicsWithValue(t, "synthetic preview panic", func() {
		_, _ = b.processPage(t.Context(), []string{"a"}, newBackfillRetrySet())
	})
}

func TestControlledBackfillCancellationAndRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		b := Backfill[string]{Key: func(s string) string { return s }, Control: func(context.Context) (store.LaneControl, error) {
			return store.LaneControl{Paused: true, Concurrency: 1}, nil
		}, Process: func(context.Context, string) error { t.Fatal("paused target dispatched"); return nil }}
		done := make(chan error, 1)
		go func() { _, err := b.processPage(ctx, []string{"a"}, newBackfillRetrySet()); done <- err }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		catalog := newFakeBackfillCatalog("a", "b")
		catalog.failing["a"] = 1
		backfill := newTestBackfill(catalog, 2, true)
		backfill.Control = func(context.Context) (store.LaneControl, error) { return store.LaneControl{Concurrency: 2}, nil }
		require.NoError(t, backfill.Run(t.Context()))
		require.Equal(t, 2, catalog.attempts["a"])
		require.Equal(t, 1, catalog.attempts["b"])
		workerFailure := errors.New("active preview failed")
		calls := 0
		drained := Backfill[string]{Key: func(s string) string { return s }, Control: func(context.Context) (store.LaneControl, error) {
			calls++
			if calls == 1 {
				return store.LaneControl{Concurrency: 1}, nil
			}
			return store.LaneControl{}, errors.New("read failed")
		}, Process: func(context.Context, string) error { return workerFailure }}
		_, err := drained.processPage(t.Context(), []string{"active", "next"}, newBackfillRetrySet())
		require.ErrorIs(t, err, workerFailure, "Run logs failures drained after a control error")
		require.ErrorIs(t, err, errBackfillControl)
	})
}

func TestControlledBackfillRelistsPageAfterControlFailure(t *testing.T) {
	t.Parallel()
	t.Run("relist order", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			catalog := newFakeBackfillCatalog("a", "b", "c", "d")
			backfill := newTestBackfill(catalog, 2, true)
			var order []string
			process := backfill.Process
			backfill.Process = func(ctx context.Context, key string) error {
				order = append(order, key)
				return process(ctx, key)
			}
			var calls atomic.Int32
			backfill.Control = func(context.Context) (store.LaneControl, error) {
				if calls.Add(1) == 2 {
					return store.LaneControl{}, errors.New("read failed")
				}
				return store.LaneControl{Concurrency: 1}, nil
			}
			require.NoError(t, backfill.Run(t.Context()))
			require.Equal(t, []string{"a", "b", "c", "d"}, order)
		})
	})
	t.Run("backoff duration", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			catalog := newFakeBackfillCatalog("a")
			catalog.failing["a"] = 1
			backfill := newTestBackfill(catalog, 2, false)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			backfill.Control = func(context.Context) (store.LaneControl, error) {
				switch n := calls.Add(1); {
				case n == 1:
					return store.LaneControl{Concurrency: 1}, nil
				case n > 10:
					cancel()
				}
				return store.LaneControl{}, errors.New("read failed")
			}
			started := time.Now()
			require.ErrorIs(t, backfill.Run(ctx), context.Canceled)
			require.GreaterOrEqual(t, time.Since(started), 9*backfillListRetryDelay,
				"each control failure waits before relisting, even with an overdue retry")
		})
	})
}
