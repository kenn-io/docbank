// Package gitlock serializes destructive git repository mutations.
//
// Git worktree add, remove, prune, fetch, and branch updates can all mutate
// shared repository metadata. Manager combines an in-process semaphore with an
// on-disk flock so callers can protect those operations both within one
// process and across cooperating processes.
package gitlock

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sync/semaphore"
)

// Locker is a held repository lock. Unlock must be called exactly once.
type Locker interface {
	Unlock() error
}

// Manager combines an in-process semaphore with an on-disk flock.
//
// Both layers are intentional: a shared flock instance can be reentrant inside
// one process, while separate processes still need a file lock when they mutate
// the same bare clone or worktree metadata.
type Manager struct {
	// FileName is the lock file name stored under each repository root.
	FileName string

	mu     sync.Mutex
	states map[string]*state
}

type state struct {
	local *semaphore.Weighted
	file  *flock.Flock
}

// New returns a lock manager. If fileName is empty, ".git-worktree.lock" is used.
func New(fileName string) *Manager {
	if fileName == "" {
		fileName = ".git-worktree.lock"
	}
	return &Manager{FileName: fileName, states: map[string]*state{}}
}

func (m *Manager) stateFor(lockPath string) *state {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.states[lockPath]; ok {
		return s
	}
	s := &state{local: semaphore.NewWeighted(1), file: flock.New(lockPath)}
	m.states[lockPath] = s
	return s
}

// Acquire locks repoRoot until ctx is done.
func (m *Manager) Acquire(ctx context.Context, repoRoot string) (Locker, error) {
	lockPath := filepath.Join(repoRoot, m.FileName)
	s := m.stateFor(lockPath)
	if err := s.local.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("acquire git lock %q: %w", repoRoot, err)
	}
	locked, err := s.file.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		s.local.Release(1)
		return nil, fmt.Errorf("acquire git lock %q: %w", repoRoot, err)
	}
	if !locked {
		s.local.Release(1)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("acquire git lock %q: %w", repoRoot, err)
		}
		return nil, fmt.Errorf("acquire git lock %q: lock not acquired", repoRoot)
	}
	return &handle{state: s}, nil
}

type handle struct {
	state    *state
	released bool
	mu       sync.Mutex
}

// Unlock releases the file lock and in-process semaphore.
func (h *handle) Unlock() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return errors.New("git lock already released")
	}
	h.released = true
	err := h.state.file.Unlock()
	h.state.local.Release(1)
	if err != nil {
		return fmt.Errorf("release git lock: %w", err)
	}
	return nil
}
