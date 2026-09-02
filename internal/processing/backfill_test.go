package processing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestBackfillDrainsEveryTargetAcrossPagesAndStops(t *testing.T) {
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
}

func TestBackfillQuarantinesFailingTargetsWithoutBlockingOthers(t *testing.T) {
	catalog := newFakeBackfillCatalog("a", "b", "c")
	catalog.failing["b"] = 1
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	backfill := newTestBackfill(catalog, 10, true)
	backfill.Now = func() time.Time { return now }

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- backfill.Run(ctx) }()

	require.Eventually(t, func() bool {
		catalog.mu.Lock()
		defer catalog.mu.Unlock()
		return catalog.done["a"] && catalog.done["c"] && catalog.attempts["b"] == 1
	}, time.Second, 5*time.Millisecond, "siblings progress while b is quarantined")
	now = now.Add(backfillFirstRetryDelay)
	require.NoError(t, <-done, "the retry of b succeeds and the drain completes")
	assert.True(t, catalog.done["b"])
	assert.Equal(t, 2, catalog.attempts["b"])
}

func TestBackfillRetriesAListingFailure(t *testing.T) {
	catalog := newFakeBackfillCatalog("a")
	catalog.listErrs = 1
	backfill := newTestBackfill(catalog, 10, true)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, backfill.Run(ctx))
	assert.True(t, catalog.done["a"])
	assert.GreaterOrEqual(t, catalog.listed, 2)
}

func TestBackfillKeepsWatchingWhenNotDraining(t *testing.T) {
	catalog := newFakeBackfillCatalog("a")
	backfill := newTestBackfill(catalog, 10, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- backfill.Run(ctx) }()
	require.Eventually(t, func() bool {
		catalog.mu.Lock()
		defer catalog.mu.Unlock()
		return catalog.done["a"] && catalog.listed >= 3
	}, time.Second, 5*time.Millisecond, "an empty scan is followed by another scan")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestBackfillRejectsIncompleteConfiguration(t *testing.T) {
	backfill := &Backfill[string]{Page: 1, IdleDelay: time.Millisecond}
	require.Error(t, backfill.Run(t.Context()))
}

func TestBackfillRetrySetDropsTargetsAbsentFromCompletedScan(t *testing.T) {
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
