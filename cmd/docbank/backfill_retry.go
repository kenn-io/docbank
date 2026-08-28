package main

import "time"

const maxBackfillRetryDelay = 10 * time.Minute

type backfillRetryState struct {
	failures  uint
	notBefore time.Time
}

type backfillRetrySet map[string]backfillRetryState

func newBackfillRetrySet() backfillRetrySet {
	return make(backfillRetrySet)
}

func (retries backfillRetrySet) ready(key string, now time.Time) bool {
	state, found := retries[key]
	return !found || !now.Before(state.notBefore)
}

func (retries backfillRetrySet) failed(key string, now time.Time) {
	state := retries[key]
	state.failures++
	delay := 5 * time.Second
	for attempt := uint(1); attempt < state.failures && delay < maxBackfillRetryDelay; attempt++ {
		delay = min(delay*2, maxBackfillRetryDelay)
	}
	state.notBefore = now.Add(delay)
	retries[key] = state
}

func (retries backfillRetrySet) succeeded(key string) {
	delete(retries, key)
}

func (retries backfillRetrySet) waitDelay(now time.Time, maximum time.Duration) time.Duration {
	delay := maximum
	for _, state := range retries {
		if !now.Before(state.notBefore) {
			return 0
		}
		delay = min(delay, state.notBefore.Sub(now))
	}
	return delay
}

func backfillBatchWaitDelay(
	attempted int, retries backfillRetrySet, now time.Time,
) time.Duration {
	if attempted != 0 || len(retries) == 0 {
		return 100 * time.Millisecond
	}
	return retries.waitDelay(now, 10*time.Second)
}
