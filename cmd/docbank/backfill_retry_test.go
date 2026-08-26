package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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

func TestBackfillBatchWaitHonorsRetryWhenEveryTargetIsSkipped(t *testing.T) {
	retries := newBackfillRetrySet()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	retries.failed("deferred", now)

	assert.Equal(t, 5*time.Second,
		backfillBatchWaitDelay(0, retries, now))
	assert.Equal(t, 100*time.Millisecond,
		backfillBatchWaitDelay(1, retries, now))
}
