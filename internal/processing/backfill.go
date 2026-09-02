package processing

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	backfillFirstRetryDelay = 5 * time.Second
	backfillMaxRetryDelay   = 10 * time.Minute
	backfillListRetryDelay  = 5 * time.Second
	backfillBatchPause      = 100 * time.Millisecond
	backfillMaxIdleDelay    = 10 * time.Second
)

// Backfill walks a catalog listing in key order, processes every target under
// the daemon's mutation gate, and retries failed targets with a per-target
// backoff so one bad original cannot pin the whole scan. It is the one
// daemon loop shape for catalog-driven derived data: auxiliary checksums and
// source metadata both run through it.
type Backfill[T any] struct {
	// Name labels log lines.
	Name string
	// Page is the number of targets listed per catalog call.
	Page int
	// IdleDelay is the first wait after an empty scan; it doubles up to ten
	// seconds while the scan stays empty.
	IdleDelay time.Duration
	// DrainOnce makes Run return nil once a scan finds nothing to do and no
	// retries are pending, for backfills that only cover pre-existing rows.
	DrainOnce bool
	// List returns targets ordered by key after the given cursor.
	List func(ctx context.Context, after string, limit int) ([]T, error)
	// Key returns the ordering key of one target, which is also its cursor.
	Key func(T) string
	// Process handles one target; its error schedules a retry.
	Process func(ctx context.Context, target T) error
	// Mutate runs fn on the shared side of the maintenance gate. It may be
	// nil when no gate applies.
	Mutate func(ctx context.Context, fn func() error) error
	// Logger receives retry warnings. It may be nil.
	Logger *slog.Logger
	// Now supplies the clock; nil means time.Now.
	Now func() time.Time
}

// Run executes the backfill until ctx ends, or until the scan drains when
// DrainOnce is set.
func (b *Backfill[T]) Run(ctx context.Context) error {
	if err := b.validate(); err != nil {
		return err
	}
	retries := newBackfillRetrySet()
	cursor := ""
	idleDelay := b.IdleDelay
	for {
		targets, err := b.List(ctx, cursor, b.Page)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.warn("listing backfill targets will retry", "error", err)
			if err := waitBackfill(ctx, backfillListRetryDelay); err != nil {
				return err
			}
			continue
		}
		if len(targets) == 0 {
			cursor = ""
			retries.retainSeen()
			if b.DrainOnce && len(retries) == 0 {
				return nil
			}
			if err := waitBackfill(ctx, retries.waitDelay(b.now(), idleDelay)); err != nil {
				return err
			}
			idleDelay = min(idleDelay*2, backfillMaxIdleDelay)
			continue
		}
		cursor = b.Key(targets[len(targets)-1])
		idleDelay = b.IdleDelay
		attempted, err := b.processPage(ctx, targets, retries)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.warn("backfill will retry", "error", err)
		}
		if attempted == len(targets) && len(targets) == b.Page {
			continue
		}
		delay := backfillBatchPause
		if attempted == 0 {
			delay = retries.waitDelay(b.now(), backfillMaxIdleDelay)
		}
		if err := waitBackfill(ctx, delay); err != nil {
			return err
		}
	}
}

func (b *Backfill[T]) processPage(
	ctx context.Context, targets []T, retries backfillRetrySet,
) (int, error) {
	attempted := 0
	var pageErr error
	for _, target := range targets {
		key := b.Key(target)
		now := b.now()
		if !retries.ready(key, now) {
			retries.seen(key)
			continue
		}
		attempted++
		err := b.mutate(ctx, func() error { return b.Process(ctx, target) })
		if err != nil {
			if ctx.Err() != nil {
				return attempted, ctx.Err()
			}
			retries.failed(key, now)
			pageErr = errors.Join(pageErr, err)
			continue
		}
		retries.succeeded(key)
	}
	return attempted, pageErr
}

func (b *Backfill[T]) validate() error {
	if b.List == nil || b.Key == nil || b.Process == nil {
		return errors.New("backfill requires List, Key, and Process")
	}
	if b.Page <= 0 || b.IdleDelay <= 0 {
		return errors.New("backfill requires a positive page size and idle delay")
	}
	return nil
}

func (b *Backfill[T]) mutate(ctx context.Context, fn func() error) error {
	if b.Mutate == nil {
		return fn()
	}
	return b.Mutate(ctx, fn)
}

func (b *Backfill[T]) now() time.Time {
	if b.Now == nil {
		return time.Now().UTC()
	}
	return b.Now()
}

func (b *Backfill[T]) warn(msg string, args ...any) {
	if b.Logger != nil {
		b.Logger.Warn(msg, append([]any{"backfill", b.Name}, args...)...)
	}
}

func waitBackfill(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type backfillRetryState struct {
	failures  uint
	notBefore time.Time
	// seenThisScan records that the current scan still lists the target, so
	// retries for rows that disappeared are dropped when the scan drains.
	seenThisScan bool
}

// backfillRetrySet quarantines failing targets with exponential backoff
// without blocking unrelated targets in the same page.
type backfillRetrySet map[string]*backfillRetryState

func newBackfillRetrySet() backfillRetrySet {
	return make(backfillRetrySet)
}

func (retries backfillRetrySet) ready(key string, now time.Time) bool {
	state, found := retries[key]
	return !found || !now.Before(state.notBefore)
}

func (retries backfillRetrySet) seen(key string) {
	if state, found := retries[key]; found {
		state.seenThisScan = true
	}
}

func (retries backfillRetrySet) failed(key string, now time.Time) {
	state, found := retries[key]
	if !found {
		state = &backfillRetryState{}
		retries[key] = state
	}
	state.failures++
	state.seenThisScan = true
	delay := backfillFirstRetryDelay
	for attempt := uint(1); attempt < state.failures && delay < backfillMaxRetryDelay; attempt++ {
		delay = min(delay*2, backfillMaxRetryDelay)
	}
	state.notBefore = now.Add(delay)
}

func (retries backfillRetrySet) succeeded(key string) {
	delete(retries, key)
}

// retainSeen drops retries for targets the completed scan no longer listed
// and resets the marks for the next scan.
func (retries backfillRetrySet) retainSeen() {
	for key, state := range retries {
		if !state.seenThisScan {
			delete(retries, key)
			continue
		}
		state.seenThisScan = false
	}
}

// waitDelay returns the time until the earliest retry, zero when one is
// already due, or fallback when nothing is waiting.
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
