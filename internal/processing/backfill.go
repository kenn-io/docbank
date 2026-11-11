package processing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

const (
	backfillFirstRetryDelay = 5 * time.Second
	backfillMaxRetryDelay   = 10 * time.Minute
	backfillListRetryDelay  = 5 * time.Second
	backfillBatchPause      = 100 * time.Millisecond
	backfillMaxIdleDelay    = 10 * time.Second
	backfillDrainAttempts   = 3
)

var errBackfillControl = errors.New("reading backfill lane control")
var errBackfillPage = errors.New("invalid backfill page")

// BackfillPage separates scan progress from actionable targets. A filtered
// page still advances Next. EOF is a separate empty page with no Next cursor.
type BackfillPage[T any] struct {
	Targets []T
	Next    string
	EOF     bool
}

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
	// seconds while the scan stays empty unless MaxIdleDelay is set.
	IdleDelay time.Duration
	// MaxIdleDelay caps the empty-scan wait; zero defaults to ten seconds.
	MaxIdleDelay time.Duration
	// DrainOnce makes Run return nil once a scan finds nothing to do and no
	// retries are pending, for backfills that only cover pre-existing rows.
	// Three consecutive listing failures or three failures of one target return
	// the last error so synchronous callers do not wait indefinitely.
	DrainOnce bool
	// WriteRevision changes after committed catalog writes. A complete no-action
	// pass with an unchanged revision settles until the next write. DrainOnce ignores it.
	WriteRevision func() uint64
	// Busy reports work whose completion does not change WriteRevision.
	// A busy lane keeps listing until that work finishes.
	Busy func() bool
	// List returns targets ordered by key after the given cursor.
	List func(ctx context.Context, after string, limit int) ([]T, error)
	// ListPage optionally replaces List when filtering can produce empty pages
	// before EOF. Configure exactly one listing callback. Keys and Next use the
	// same strictly increasing ordering; targets cannot exceed Next or Page.
	ListPage func(ctx context.Context, after string, limit int) (BackfillPage[T], error)
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
	// Control supplies pause and concurrency settings; nil means unpaused with
	// concurrency 1.
	Control func(context.Context) (store.LaneControl, error)
}

// Run executes the backfill until ctx ends, or until the scan drains when
// DrainOnce is set.
func (b *Backfill[T]) Run(ctx context.Context) error {
	jobs.ReportBusy(ctx, false)
	if err := b.validate(); err != nil {
		return err
	}
	retries := newBackfillRetrySet()
	cursor := ""
	idleDelay := b.IdleDelay
	maxIdleDelay := b.MaxIdleDelay
	if maxIdleDelay <= 0 {
		maxIdleDelay = backfillMaxIdleDelay
	}
	listFailures := 0
	// passFound records whether the scan since the cursor last reset attempted
	// any target.
	passFound := false
	var settled bool
	var settledRevision uint64
	var passRevision uint64
	var passStarted bool
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !passStarted && !b.DrainOnce && b.WriteRevision != nil {
			passRevision = b.WriteRevision()
			if passRevision != settledRevision {
				settled = false
			}
		}
		passStarted = true
		page := BackfillPage[T]{EOF: true}
		var err error
		if !settled {
			page, err = b.scan(ctx, cursor)
		}
		if err != nil {
			if errors.Is(err, errBackfillPage) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			listFailures++
			if b.DrainOnce && listFailures >= backfillDrainAttempts {
				return err
			}
			b.warn("listing backfill targets will retry", "error", err)
			// Only the next attempt can tell whether listing works again.
			jobs.ReportRetryPending(ctx, true)
			if err := jobs.Idle(ctx, backfillListRetryDelay, false); err != nil {
				return err
			}
			continue
		}
		listFailures = 0
		if page.EOF {
			cursor = ""
			passStarted = false
			retries.retainSeen()
			if b.DrainOnce && len(retries) == 0 {
				return nil
			}
			changed := !b.DrainOnce && b.WriteRevision != nil && b.WriteRevision() != passRevision
			if !b.DrainOnce && b.WriteRevision != nil && !passFound && len(retries) == 0 &&
				(b.Busy == nil || !b.Busy()) && !changed {
				settled, settledRevision = true, passRevision
			}
			// Only a completed scan reports no work to Settle. Targets in retry
			// backoff become ready only with time.
			jobs.ReportRetryPending(ctx, len(retries) > 0)
			if err := jobs.Idle(ctx, retries.waitDelay(b.now(), idleDelay), passFound || changed); err != nil {
				return err
			}
			passFound = false
			idleDelay = min(idleDelay*2, maxIdleDelay)
			continue
		}
		targets := page.Targets
		if len(targets) == 0 {
			cursor = page.Next
			if err := jobs.Idle(ctx, backfillBatchPause, true); err != nil {
				return err
			}
			continue
		}
		idleDelay = b.IdleDelay
		attempted, err := b.processPage(ctx, targets, retries)
		passFound = passFound || attempted > 0
		// A failed control read can stop mid-page; relist it instead of skipping the rest.
		if !errors.Is(err, errBackfillControl) {
			cursor = page.Next
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if b.DrainOnce {
				for _, retry := range retries {
					if retry.failures >= backfillDrainAttempts {
						return err
					}
				}
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
		// Later pages are still unread, so a pause inside a scan reports found
		// work: Settle wakes the backfill to read on instead of waiting it out.
		found := true
		if errors.Is(err, errBackfillControl) {
			// The page is relisted, so an overdue retry must not make this a busy
			// loop. Only the next control read can tell whether it works again.
			delay = backfillListRetryDelay
			found = false
			jobs.ReportRetryPending(ctx, true)
		}
		if err := jobs.Idle(ctx, delay, found); err != nil {
			return err
		}
	}
}

// processPage dispatches ready targets in key order while the lane is
// unpaused and below its concurrency, rereading the control before each
// dispatch and every poll interval while it waits.
func (b *Backfill[T]) processPage(
	ctx context.Context, targets []T, retries backfillRetrySet,
) (attempted int, pageErr error) {
	type result struct {
		key       string
		started   time.Time
		err       error
		recovered any
	}
	results := make(chan result, len(targets))
	active, next := 0, 0
	var recovered any
	record := func(done result) {
		active--
		jobs.ReportBusy(ctx, active > 0)
		if done.recovered != nil {
			recovered = done.recovered
			next = len(targets)
		}
		if done.err != nil {
			retries.failed(done.key, done.started)
			pageErr = errors.Join(pageErr, done.err)
		} else {
			retries.succeeded(done.key)
		}
	}
	defer func() {
		for active > 0 {
			record(<-results)
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	for {
		for next < len(targets) && !retries.ready(b.Key(targets[next]), b.now()) {
			retries.seen(b.Key(targets[next]))
			next++
		}
		if next == len(targets) && active == 0 {
			return attempted, pageErr
		}
		if err := ctx.Err(); err != nil {
			return attempted, err
		}
		var poll <-chan time.Time
		if next < len(targets) {
			control, err := b.control(ctx)
			if err != nil {
				return attempted, errors.Join(pageErr, err)
			}
			if !control.Paused && active < control.Concurrency {
				target := targets[next]
				next++
				active++
				jobs.ReportBusy(ctx, true)
				attempted++
				go func(done result) {
					defer func() {
						done.recovered = recover()
						results <- done
					}()
					done.err = b.mutate(ctx, func() error { return b.Process(ctx, target) })
				}(result{key: b.Key(target), started: b.now()})
				continue
			}
			poll = time.After(jobs.LanePollInterval)
		}
		select {
		case done := <-results:
			record(done)
		case <-ctx.Done():
			return attempted, ctx.Err()
		case <-poll:
		}
	}
}

// control reports the lane settings; a Backfill without Control runs one
// target at a time and never pauses.
func (b *Backfill[T]) control(ctx context.Context) (store.LaneControl, error) {
	if b.Control == nil {
		return store.LaneControl{Concurrency: 1}, nil
	}
	control, err := b.Control(ctx)
	if err != nil {
		return store.LaneControl{}, fmt.Errorf("%w: %w", errBackfillControl, err)
	}
	return control, nil
}

func (b *Backfill[T]) validate() error {
	if (b.List == nil) == (b.ListPage == nil) || b.Key == nil || b.Process == nil {
		return errors.New("backfill requires exactly one of List or ListPage, plus Key and Process")
	}
	if b.Page <= 0 || b.IdleDelay <= 0 {
		return errors.New("backfill requires a positive page size and idle delay")
	}
	return nil
}

// scan adapts legacy listings into the same progress/EOF contract as filtered
// pages. Invalid provider progress is a contract error, not retryable I/O.
func (b *Backfill[T]) scan(ctx context.Context, after string) (BackfillPage[T], error) {
	var page BackfillPage[T]
	var err error
	if b.ListPage != nil {
		page, err = b.ListPage(ctx, after, b.Page)
	} else {
		page.Targets, err = b.List(ctx, after, b.Page)
		page.EOF = len(page.Targets) == 0
		if !page.EOF {
			page.Next = b.Key(page.Targets[len(page.Targets)-1])
		}
	}
	if err != nil {
		return page, err
	}
	if len(page.Targets) > b.Page || (page.EOF && (len(page.Targets) != 0 || page.Next != "")) || (!page.EOF && page.Next <= after) {
		return page, errBackfillPage
	}
	previous := after
	for _, target := range page.Targets {
		key := b.Key(target)
		if key <= previous || key > page.Next {
			return page, errBackfillPage
		}
		previous = key
	}
	return page, nil
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
