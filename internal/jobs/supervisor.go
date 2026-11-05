// Package jobs supervises daemon-owned background work.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrClosed    = errors.New("background job supervisor is stopping")
	ErrDuplicate = errors.New("background job name is already registered")
	ErrInvalid   = errors.New("invalid background job")
)

// Status is the current terminal or non-terminal state of one named job.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Snapshot is a stable copy of one job's observable state.
type Snapshot struct {
	Name       string
	Status     Status
	Busy       bool
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

type state struct {
	Snapshot

	transient bool
	// worker marks a polling worker that parks in Idle between polls.
	worker bool
	// wake is non-nil while the worker is parked in Idle. Settle closes it.
	wake chan struct{}
	// pollRound is the settle round in which the worker's current or latest
	// poll began.
	pollRound uint64
	// retryPending records the worker's latest ReportRetryPending.
	retryPending bool
}

type reportErrorKey struct{}
type reportBusyKey struct{}
type idleKey struct{}
type reportFoundKey struct{}
type reportRetryKey struct{}

// ReportRetryPending records whether a worker holds work that only a later
// retry time can make ready, such as a target in retry backoff. Settle cannot
// finish that work early, so it treats the worker as settled and names it in
// Supervisor.PendingRetries. Calls outside a job started with StartWorker have
// no effect.
func ReportRetryPending(ctx context.Context, pending bool) {
	if report, ok := ctx.Value(reportRetryKey{}).(func(bool)); ok {
		report(pending)
	}
}

// ReportFound records that a worker did work, for a worker whose waits are far
// from the work, so it parks with found set to false. Settle then runs another
// round. Calls outside a job started with StartWorker have no effect.
func ReportFound(ctx context.Context) {
	if report, ok := ctx.Value(reportFoundKey{}).(func()); ok {
		report()
	}
}

// Idle waits delay before a polling worker polls again, or until ctx ends.
// found reports whether the polls since the worker last waited did any work.
// Inside a job started with StartWorker, Supervisor.Settle can end the wait
// early. Idle waits only on a timer and channels, so it runs on fake time in a
// synctest bubble.
func Idle(ctx context.Context, delay time.Duration, found bool) error {
	settle, resume := Park(ctx, found)
	defer resume()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-settle:
		return nil
	}
}

// Park lets a worker that waits in its own select take part in Settle, as
// Idle does. found has the same meaning as for Idle. The worker adds the
// returned channel to its select; Settle closes it to ask for another poll.
// The worker calls resume when it starts its next poll. Outside a job started
// with StartWorker, the channel is nil and resume does nothing.
func Park(ctx context.Context, found bool) (settle <-chan struct{}, resume func()) {
	if park, ok := ctx.Value(idleKey{}).(func(bool) (<-chan struct{}, func())); ok {
		return park(found)
	}
	return nil, func() {}
}

// ReportBusy distinguishes processing an item from waiting for work. Long-lived
// workers report false before polling and true only while processing items.
// Calls outside a supervised job have no effect.
func ReportBusy(ctx context.Context, busy bool) {
	if report, ok := ctx.Value(reportBusyKey{}).(func(bool)); ok {
		report(busy)
	}
}

// ReportError updates a running job's observable error. A nil error clears it.
// Calls outside a supervised job have no effect.
func ReportError(ctx context.Context, err error) {
	if report, ok := ctx.Value(reportErrorKey{}).(func(error)); ok {
		report(err)
	}
}

// Supervisor owns uniquely named goroutines. Stop prevents new
// jobs and cancels the shared root; Shutdown additionally waits for every job
// to return before the daemon closes resources they may use.
type Supervisor struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger *slog.Logger

	mu       sync.Mutex
	jobs     map[string]*state
	active   int
	stopping bool
	done     chan struct{}
	doneOnce sync.Once

	// round counts Settle rounds. roundFound records that a worker found work
	// since the current round began.
	round      uint64
	roundFound bool
	// changed is closed and replaced whenever a worker parks or a job ends.
	changed chan struct{}
}

func New(parent context.Context, logger *slog.Logger) *Supervisor {
	if parent == nil {
		panic("jobs: nil parent context")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Supervisor{
		ctx: ctx, cancel: cancel, logger: logger, jobs: make(map[string]*state), done: make(chan struct{}),
		changed: make(chan struct{}),
	}
}

// Start registers and launches one job. Names remain reserved after a job
// finishes so status does not silently switch to a different run.
func (s *Supervisor) Start(name string, run func(context.Context) error) error {
	return s.start(name, run, false, false)
}

// StartWorker starts a job that Settle waits for: a long-lived polling worker
// that waits in Idle or Park between polls, or a one-shot operation that never
// parks, which Settle waits for until it returns.
func (s *Supervisor) StartWorker(name string, run func(context.Context) error) error {
	return s.start(name, run, false, true)
}

// StartTransient registers a job only until its runner returns. Shutdown still
// cancels and waits for it, but completed requests do not remain in Snapshot.
func (s *Supervisor) StartTransient(name string, run func(context.Context) error) error {
	return s.start(name, run, true, false)
}

func (s *Supervisor) start(name string, run func(context.Context) error, transient, worker bool) error {
	if err := validate(name, run); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.ctx.Err() != nil {
		return ErrClosed
	}
	if _, exists := s.jobs[name]; exists {
		return fmt.Errorf("job %q: %w", name, ErrDuplicate)
	}
	job := &state{
		Name: name, Status: StatusRunning, Busy: true, StartedAt: time.Now().UTC(),
		transient: transient, worker: worker}
	s.jobs[name] = job
	s.active++
	go s.run(job, run)
	return nil
}

func (s *Supervisor) run(job *state, run func(context.Context) error) {
	var runErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			runErr = fmt.Errorf("panic: %v", recovered)
			s.logger.Error("background job panicked", "job", job.Name,
				"panic", recovered, "stack", string(debug.Stack()))
		}
		s.finish(job, runErr)
	}()
	ctx := context.WithValue(s.ctx, reportErrorKey{}, func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		job.Error = ""
		if err != nil {
			job.Error = boundedError(err)
		}
	})
	ctx = context.WithValue(ctx, reportBusyKey{}, func(busy bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		job.Busy = busy
	})
	if job.worker {
		ctx = context.WithValue(ctx, idleKey{}, func(found bool) (<-chan struct{}, func()) {
			return s.park(job, found)
		})
		ctx = context.WithValue(ctx, reportFoundKey{}, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.roundFound = true
		})
		ctx = context.WithValue(ctx, reportRetryKey{}, func(pending bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			job.retryPending = pending
		})
	}
	runErr = run(ctx)
}

// park records that a worker waits in Idle. The returned function records the
// start of its next poll.
func (s *Supervisor) park(job *state, found bool) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if found {
		s.roundFound = true
	}
	wake := make(chan struct{})
	job.wake = wake
	s.notifyLocked()
	return wake, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if job.wake == wake {
			job.wake = nil
		}
		job.pollRound = s.round
	}
}

func (s *Supervisor) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Wake asks every parked worker started with StartWorker to poll now, without
// waiting for the result. A test uses it when the work it starts blocks on
// something the test still has to release, so Settle could not return yet.
func (s *Supervisor) Wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.worker && job.wake != nil {
			close(job.wake)
			job.wake = nil
		}
	}
}

// PendingRetries names the running workers that last reported work waiting
// for a retry time. Settle returns without that work, so a caller can check it.
func (s *Supervisor) PendingRetries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, job := range s.jobs {
		if job.worker && job.Status == StatusRunning && job.retryPending {
			names = append(names, job.Name)
		}
	}
	sort.Strings(names)
	return names
}

// Settle wakes every polling worker started with StartWorker and returns once a
// complete round finishes in which each of them polled after the round began
// and found no work. A worker that finds work starts another round, so work
// that one worker hands to another is finished too. Work that waits for a
// retry time is not finished; see PendingRetries. Settle does not wait for
// jobs started with Start or StartTransient.
func (s *Supervisor) Settle(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		s.round++
		s.roundFound = false
		for {
			settled := true
			for _, job := range s.jobs {
				if !job.worker || job.Status != StatusRunning {
					continue
				}
				switch {
				case job.wake == nil:
					// The worker is polling.
					settled = false
				case job.pollRound < s.round:
					// Its latest poll began before this round, so it may have
					// missed work queued since. Wake it to poll again.
					close(job.wake)
					job.wake = nil
					settled = false
				}
			}
			if settled {
				break
			}
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				s.mu.Lock()
				var polling []string
				for _, job := range s.jobs {
					if job.worker && job.Status == StatusRunning && job.wake == nil {
						polling = append(polling, job.Name)
					}
				}
				sort.Strings(polling)
				return fmt.Errorf("settling background workers (still polling: %s): %w",
					strings.Join(polling, ", "), ctx.Err())
			}
			s.mu.Lock()
		}
		if !s.roundFound {
			return nil
		}
	}
}

func (s *Supervisor) finish(job *state, runErr error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	job.FinishedAt = &now
	job.Busy = false
	switch {
	case runErr == nil && s.ctx.Err() == nil:
		job.Status = StatusCompleted
	case runErr == nil || errors.Is(runErr, context.Canceled):
		job.Status = StatusCancelled
	default:
		job.Status = StatusFailed
		job.Error = boundedError(runErr)
		s.logger.Error("background job failed", "job", job.Name, "error", runErr)
	}
	if job.transient {
		delete(s.jobs, job.Name)
	}
	job.wake = nil
	s.notifyLocked()
	s.active--
	if s.stopping && s.active == 0 {
		s.doneOnce.Do(func() { close(s.done) })
	}
}

// Stop requests cancellation without waiting.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		s.cancel()
	}
	if s.active == 0 {
		s.doneOnce.Do(func() { close(s.done) })
	}
	s.mu.Unlock()
}

// Shutdown stops the supervisor and waits for every registered job.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("jobs: nil shutdown context")
	}
	s.Stop()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for background jobs: %w", ctx.Err())
	}
}

// Snapshot returns every registered job sorted by stable name.
func (s *Supervisor) Snapshot() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, 0, len(s.jobs))
	for _, job := range s.jobs {
		snapshot := job.Snapshot
		if job.FinishedAt != nil {
			finished := *job.FinishedAt
			snapshot.FinishedAt = &finished
		}
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func validate(name string, run func(context.Context) error) error {
	if run == nil {
		return fmt.Errorf("job %q has no runner: %w", name, ErrInvalid)
	}
	if name == "" || len(name) > 128 {
		return fmt.Errorf("job name must contain 1-128 characters: %w", ErrInvalid)
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			strings.ContainsRune("-_.:/", char) {
			continue
		}
		return fmt.Errorf("job name %q contains unsupported characters: %w", name, ErrInvalid)
	}
	return nil
}

func boundedError(err error) string {
	const maxBytes = 4096
	message := strings.ToValidUTF8(err.Error(), "\uFFFD")
	if len(message) <= maxBytes {
		return message
	}
	prefix := message[:maxBytes-3]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + "..."
}
