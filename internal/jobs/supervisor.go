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
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

type state struct {
	Snapshot
}

// Supervisor owns a fixed set of uniquely named goroutines. Stop prevents new
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
	}
}

// Start registers and launches one job. Names remain reserved after a job
// finishes so status does not silently switch to a different run.
func (s *Supervisor) Start(name string, run func(context.Context) error) error {
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
	job := &state{Snapshot: Snapshot{
		Name: name, Status: StatusRunning, StartedAt: time.Now().UTC(),
	}}
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
	runErr = run(s.ctx)
}

func (s *Supervisor) finish(job *state, runErr error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	job.FinishedAt = &now
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
