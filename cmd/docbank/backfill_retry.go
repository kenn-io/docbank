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

func (retries backfillRetrySet) retain(present map[string]struct{}) {
	for key := range retries {
		if _, found := present[key]; !found {
			delete(retries, key)
		}
	}
}

func (retries backfillRetrySet) waitDelay(now time.Time, fallback time.Duration) time.Duration {
	if len(retries) == 0 {
		return fallback
	}
	var delay time.Duration
	for _, state := range retries {
		if !now.Before(state.notBefore) {
			return 0
		}
		candidate := state.notBefore.Sub(now)
		if delay == 0 || candidate < delay {
			delay = candidate
		}
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
