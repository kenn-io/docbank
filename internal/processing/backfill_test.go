package processing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestBackfillReportsBusyUntilEveryConcurrentItemFinishes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
		defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
		var paused atomic.Bool
		paused.Store(true)
		first, second := make(chan struct{}), make(chan struct{})
		listed := false
		worker := &Backfill[string]{
			Name: "previews", Page: 2, IdleDelay: time.Second,
			List: func(context.Context, string, int) ([]string, error) {
				if listed {
					return nil, nil
				}
				listed = true
				return []string{"a", "b"}, nil
			},
			Key: func(key string) string { return key },
			Control: func(context.Context) (store.LaneControl, error) {
				return store.LaneControl{Paused: paused.Load(), Concurrency: 2}, nil
			},
			Process: func(ctx context.Context, key string) error {
				release := first
				if key == "b" {
					release = second
				}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}
		require.NoError(t, supervisor.Start("derive:visual-previews", worker.Run))
		synctest.Wait()
		assert.False(t, supervisor.Snapshot()[0].Busy, "paused before dispatch")
		paused.Store(false)
		time.Sleep(jobs.LanePollInterval)
		synctest.Wait()
		assert.True(t, supervisor.Snapshot()[0].Busy)
		close(first)
		synctest.Wait()
		assert.True(t, supervisor.Snapshot()[0].Busy, "second item is still processing")
		close(second)
		synctest.Wait()
		assert.False(t, supervisor.Snapshot()[0].Busy, "worker is waiting for more work")
	})
}

type fakeBackfillCatalog struct {
	mu       sync.Mutex
	keys     []string
	failing  map[string]int
	done     map[string]bool
	listErrs int
	attempts map[string]int
	listed   int
}

func (c *fakeBackfillCatalog) list(_ context.Context, after string, limit int) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listed++
	if c.listErrs > 0 {
		c.listErrs--
		return nil, errors.New("synthetic list failure")
	}
	var page []string
	for _, key := range c.keys {
		if key > after && !c.done[key] {
			page = append(page, key)
			if len(page) == limit {
				break
			}
		}
	}
	return page, nil
}

func (c *fakeBackfillCatalog) process(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[key]++
	if c.failing[key] > 0 {
		c.failing[key]--
		return errors.New("synthetic failure for " + key)
	}
	c.done[key] = true
	return nil
}

func newFakeBackfillCatalog(keys ...string) *fakeBackfillCatalog {
	return &fakeBackfillCatalog{
		keys: keys, failing: map[string]int{}, done: map[string]bool{}, attempts: map[string]int{},
	}
}

func newTestBackfill(catalog *fakeBackfillCatalog, page int, drain bool) *Backfill[string] {
	return &Backfill[string]{
		Name: "test", Page: page, IdleDelay: time.Millisecond, DrainOnce: drain,
		List: catalog.list, Key: func(key string) string { return key }, Process: catalog.process,
	}
}

func (c *fakeBackfillCatalog) scan(_ context.Context, after string, limit int) (BackfillPage[string], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listed++
	page := BackfillPage[string]{EOF: true}
	selected := 0
	for _, key := range c.keys {
		if key <= after {
			continue
		}
		page.EOF, page.Next = false, key
		if !c.done[key] {
			page.Targets = append(page.Targets, key)
		}
		selected++
		if selected == limit {
			break
		}
	}
	return page, nil
}

func TestBackfillPageProgressPreservesControlRelistAndRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a", "b", "c", "d")
		catalog.done["a"], catalog.done["c"], catalog.failing["b"] = true, true, 1
		worker := newTestBackfill(catalog, 1, true)
		worker.List = nil
		var cursors []string
		worker.ListPage = func(ctx context.Context, after string, limit int) (BackfillPage[string], error) {
			cursors = append(cursors, after)
			return catalog.scan(ctx, after, limit)
		}
		controls, gated := 0, 0
		worker.Control = func(context.Context) (store.LaneControl, error) {
			controls++
			if controls == 2 {
				return store.LaneControl{}, errors.New("synthetic control failure")
			}
			return store.LaneControl{Concurrency: 1}, nil
		}
		worker.Mutate = func(_ context.Context, fn func() error) error { gated++; return fn() }
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		require.NoError(t, worker.Run(ctx))
		require.Equal(t, 2, catalog.attempts["b"], "retry survives filtered pages through EOF")
		require.Equal(t, 1, catalog.attempts["d"])
		require.Equal(t, 3, gated)
		require.Equal(t, []string{"", "a", "b", "c", "c"}, cursors[:5], "control failure relists the same selected page")
	})
}

func TestBackfillFilteredPagesAdvanceWithoutPacing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a", "b", "c")
		for _, key := range catalog.keys {
			catalog.done[key] = true
		}
		worker := newTestBackfill(catalog, 1, true)
		worker.List, worker.ListPage = nil, catalog.scan
		started := time.Now()
		require.NoError(t, worker.Run(t.Context()))
		require.Equal(t, started, time.Now(), "filtered pages reach explicit EOF without a timer")
		require.Equal(t, 4, catalog.listed)
	})
}

func TestBackfillRejectsAmbiguousListingsAndInvalidPageProgress(t *testing.T) {
	t.Parallel()
	worker := newTestBackfill(newFakeBackfillCatalog(), 2, true)
	worker.ListPage = func(context.Context, string, int) (BackfillPage[string], error) {
		return BackfillPage[string]{EOF: true}, nil
	}
	require.Error(t, worker.Run(t.Context()), "two listing owners are ambiguous")
	worker.List = nil
	for _, page := range []BackfillPage[string]{
		{}, {Next: "b"}, {Next: "a"},
		{EOF: true, Next: "c"}, {EOF: true, Targets: []string{"c"}},
		{Next: "e", Targets: []string{"c", "d", "e"}},
		{Next: "e", Targets: []string{"d", "c"}},
		{Next: "d", Targets: []string{"e"}}, {Next: "d", Targets: []string{"b"}},
	} {
		worker.ListPage = func(context.Context, string, int) (BackfillPage[string], error) { return page, nil }
		_, err := worker.scan(t.Context(), "b")
		require.ErrorIs(t, err, errBackfillPage)
	}
}

func TestBackfillPagedNoActionPassSettlesOnlyAtUnchangedRevision(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a", "b")
		catalog.done["a"], catalog.done["b"] = true, true
		var revision atomic.Uint64
		worker := newTestBackfill(catalog, 1, false)
		worker.List, worker.WriteRevision = nil, revision.Load
		worker.ListPage = func(ctx context.Context, after string, limit int) (BackfillPage[string], error) {
			page, err := catalog.scan(ctx, after, limit)
			if after == "a" && revision.Load() == 0 {
				catalog.mu.Lock()
				catalog.done["a"] = false // A write behind the scan cursor.
				catalog.mu.Unlock()
				revision.Add(1)
			}
			return page, err
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- worker.Run(ctx) }()
		time.Sleep(3 * backfillMaxIdleDelay)
		synctest.Wait()
		listed := catalog.listed
		require.Equal(t, 1, catalog.attempts["a"], "a write during a filtered pass forces another full pass")
		time.Sleep(3 * backfillMaxIdleDelay)
		synctest.Wait()
		require.Equal(t, listed, catalog.listed, "unchanged multi-page scans settle at EOF")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestBackfillSettlesUntilWriteRevisionChanges(t *testing.T) {
	t.Parallel()
	t.Run("revision", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			catalog := newFakeBackfillCatalog("a")
			var revision atomic.Uint64
			backfill := newTestBackfill(catalog, 1, false)
			backfill.WriteRevision = revision.Load
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- backfill.Run(ctx) }()
			synctest.Wait()
			time.Sleep(3 * backfillMaxIdleDelay)
			synctest.Wait()
			catalog.mu.Lock()
			listed := catalog.listed
			assert.True(t, catalog.done["a"])
			catalog.mu.Unlock()
			time.Sleep(3 * backfillMaxIdleDelay)
			synctest.Wait()
			catalog.mu.Lock()
			assert.Equal(t, listed, catalog.listed, "a drained catalog stays idle")
			catalog.keys = append(catalog.keys, "b")
			catalog.mu.Unlock()
			revision.Add(1)
			time.Sleep(backfillMaxIdleDelay)
			synctest.Wait()
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			assert.True(t, catalog.done["b"], "a committed write wakes the backfill")
			assert.Equal(t, 1, catalog.attempts["b"])
		})
	})
	t.Run("supervisor", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			catalog := newFakeBackfillCatalog("a")
			backfill := newTestBackfill(catalog, 1, false)
			backfill.WriteRevision = func() uint64 { return 0 }
			listCount := func() int {
				catalog.mu.Lock()
				defer catalog.mu.Unlock()
				return catalog.listed
			}
			supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
			defer func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) }()
			require.NoError(t, supervisor.StartWorker("test", backfill.Run))
			synctest.Wait()
			time.Sleep(3 * backfillMaxIdleDelay)
			synctest.Wait()
			listed := listCount()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, supervisor.Settle(ctx))
			require.Equal(t, listed, listCount(), "settling the supervisor does not relist an unchanged catalog")
		})
	})
}

func TestBackfillCapsIdleDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog()
		backfill := newTestBackfill(catalog, 1, false)
		backfill.IdleDelay, backfill.MaxIdleDelay = time.Second, time.Second
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- backfill.Run(ctx) }()
		time.Sleep(10*time.Second + time.Millisecond)
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, 11, catalog.listed, "an empty scan waits at most one second")
	})
}

func TestBackfillBusyKeepsListingUntilReleased(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog()
		var busy atomic.Bool
		busy.Store(true)
		backfill := newTestBackfill(catalog, 1, false)
		backfill.WriteRevision = func() uint64 { return 0 }
		backfill.Busy = busy.Load
		listCount := func() int {
			catalog.mu.Lock()
			defer catalog.mu.Unlock()
			return catalog.listed
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- backfill.Run(ctx) }()
		synctest.Wait()
		first := listCount()
		time.Sleep(3 * backfillMaxIdleDelay)
		synctest.Wait()
		require.Greater(t, listCount(), first, "busy work keeps the lane polling")
		busy.Store(false)
		time.Sleep(backfillMaxIdleDelay)
		synctest.Wait()
		settled := listCount()
		time.Sleep(3 * backfillMaxIdleDelay)
		synctest.Wait()
		require.Equal(t, settled, listCount(), "the lane settles once busy work ends")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestBackfillWriteDuringEmptyListingWakesNextScan(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog()
		var revision atomic.Uint64
		backfill := newTestBackfill(catalog, 1, false)
		backfill.WriteRevision = revision.Load
		backfill.List = func(ctx context.Context, after string, limit int) ([]string, error) {
			targets, err := catalog.list(ctx, after, limit)
			if revision.Load() == 0 {
				catalog.mu.Lock()
				catalog.keys = append(catalog.keys, "a")
				catalog.mu.Unlock()
				revision.Add(1)
			}
			return targets, err
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- backfill.Run(ctx) }()
		time.Sleep(backfillMaxIdleDelay)
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		assert.True(t, catalog.done["a"], "settling uses the revision before List")
	})
}

func TestBackfillDrainsEveryTargetAcrossPagesAndStops(t *testing.T) {
	t.Parallel()
	// The pause after the partial last page passes on fake time.
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a", "b", "c", "d", "e")
		backfill := newTestBackfill(catalog, 2, true)
		gated := 0
		backfill.Mutate = func(_ context.Context, fn func() error) error { gated++; return fn() }

		require.NoError(t, backfill.Run(t.Context()))
		assert.Len(t, catalog.done, 5)
		assert.Equal(t, 5, gated, "every target runs under the gate on its own")
		for _, key := range catalog.keys {
			assert.Equal(t, 1, catalog.attempts[key], key)
		}
	})
}

func TestBackfillQuarantinesFailingTargetsWithoutBlockingOthers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a", "b", "c")
		catalog.failing["b"] = 1
		var nowMu sync.Mutex
		now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
		backfill := newTestBackfill(catalog, 10, true)
		backfill.Now = func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			return now
		}

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- backfill.Run(ctx) }()

		synctest.Wait()
		require.True(t, func() bool {
			catalog.mu.Lock()
			defer catalog.mu.Unlock()
			return catalog.done["a"] && catalog.done["c"] && catalog.attempts["b"] == 1
		}(), "siblings progress while b is quarantined")
		nowMu.Lock()
		now = now.Add(backfillFirstRetryDelay)
		nowMu.Unlock()
		require.NoError(t, <-done, "the retry of b succeeds and the drain completes")
		assert.True(t, catalog.done["b"])
		assert.Equal(t, 2, catalog.attempts["b"])
	})
}

func TestBackfillRetriesAListingFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a")
		catalog.listErrs = 1
		backfill := newTestBackfill(catalog, 10, true)

		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		require.NoError(t, backfill.Run(ctx))
		assert.True(t, catalog.done["a"])
		assert.GreaterOrEqual(t, catalog.listed, 2)
	})
}

func TestBackfillBoundsOnlyDrainRetries(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		listing bool
		drain   bool
	}{
		{name: "drain listing", listing: true, drain: true},
		{name: "drain processing", drain: true},
		{name: "watch listing", listing: true},
		{name: "watch processing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				catalog := newFakeBackfillCatalog("a")
				if testCase.listing {
					catalog.listErrs = 100000
				} else {
					catalog.failing["a"] = 100000
				}
				backfill := newTestBackfill(catalog, 1, testCase.drain)
				ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
				defer cancel()
				err := backfill.Run(ctx)
				require.Error(t, err)
				attempts := catalog.attempts["a"]
				if testCase.listing {
					attempts = catalog.listed
				}
				if !testCase.drain {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Greater(t, attempts, 3)
					return
				}
				require.NotErrorIs(t, err, context.DeadlineExceeded,
					"one-shot work must return its failure without depending on cancellation")
				require.Equal(t, 3, attempts)
			})
		})
	}
}

func TestBackfillKeepsWatchingWhenNotDraining(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		catalog := newFakeBackfillCatalog("a")
		backfill := newTestBackfill(catalog, 10, false)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- backfill.Run(ctx) }()
		synctest.Wait()
		time.Sleep(backfillBatchPause + backfill.IdleDelay)
		synctest.Wait()
		require.True(t, func() bool {
			catalog.mu.Lock()
			defer catalog.mu.Unlock()
			return catalog.done["a"] && catalog.listed >= 3
		}(), "an empty scan is followed by another scan")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestBackfillRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	backfill := &Backfill[string]{Page: 1, IdleDelay: time.Millisecond}
	require.Error(t, backfill.Run(t.Context()))
}

func TestBackfillRetrySetDropsTargetsAbsentFromCompletedScan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	retries := newBackfillRetrySet()
	retries.failed("gone", now)
	retries.failed("present", now)
	retries.retainSeen()
	retries.seen("present")
	retries.retainSeen()
	assert.Equal(t, []string{"present"}, retryKeys(retries))
	assert.Equal(t, 5*time.Second, retries.waitDelay(now, time.Second))
}

func retryKeys(retries backfillRetrySet) []string {
	keys := make([]string, 0, len(retries))
	for key := range retries {
		keys = append(keys, key)
	}
	return keys
}
