package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackfillRetrySetDropsTargetsAbsentFromCompletedScan(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	retries := newBackfillRetrySet()
	retries.failed("deleted", now)

	retries.retain(map[string]struct{}{})

	assert.Empty(t, retries)
	assert.Equal(t, 3*time.Second, retries.waitDelay(now, 3*time.Second))
}

func TestBackfillRetrySetQuarantinesFailuresWithoutBlockingOtherTargets(t *testing.T) {
	retries := newBackfillRetrySet()
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	assert.True(t, retries.ready("bad", now))
	assert.True(t, retries.ready("later", now))

	retries.failed("bad", now)
	assert.False(t, retries.ready("bad", now))
	assert.True(t, retries.ready("later", now))
	assert.True(t, retries.ready("bad", now.Add(5*time.Second)))

	retries.failed("bad", now.Add(5*time.Second))
	assert.False(t, retries.ready("bad", now.Add(14*time.Second)))
	assert.True(t, retries.ready("bad", now.Add(15*time.Second)))

	retries.succeeded("bad")
	assert.True(t, retries.ready("bad", now))
}
func TestBackfillRetrySetWaitsUntilEarliestRetry(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	retries := newBackfillRetrySet()
	retries.failed("first", now)
	retries.failed("second", now.Add(time.Second))

	assert.Equal(t, 5*time.Second, retries.waitDelay(now, time.Second))
}

func TestBackfillBatchWaitHonorsRetryWhenEveryTargetIsSkipped(t *testing.T) {
	retries := newBackfillRetrySet()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	retries.failed("deferred", now)

	assert.Equal(t, 5*time.Second,
		backfillBatchWaitDelay(0, retries, now))
	assert.Equal(t, 100*time.Millisecond,
		backfillBatchWaitDelay(1, retries, now))
}
