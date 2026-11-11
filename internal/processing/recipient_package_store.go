package processing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
	"uuid"
)

var (
	ErrRecipientPackageBusy         = errors.New("another recipient package is being prepared or downloaded; retry after it finishes or expires")
	ErrRecipientPackageUnavailable  = errors.New("recipient package is unavailable")
	ErrRecipientPackageNotPublished = errors.New("allocation has no published numbered PDF")
)

const recipientPackageLifetime = 2 * time.Minute

// PreparedRecipientPackage describes one fully checked, temporary recipient ZIP.
type PreparedRecipientPackage struct {
	ID           string                  `json:"id"`
	AllocationID uuid.UUID               `json:"allocation_id"`
	Receipt      RecipientPackageReceipt `json:"receipt"`
	Size         int64                   `json:"size"`
	SHA256       string                  `json:"sha256"`
	ExpiresAt    time.Time               `json:"expires_at"`
}

type recipientPackageStore struct {
	mu    sync.Mutex
	entry *recipientPackageEntry
	// released is closed when a pin drops, so explicit release can wait for drain.
	released chan struct{}
}

type recipientPackageEntry struct {
	owner          string
	result         PreparedRecipientPackage
	directory      string
	file           *os.File
	ready, retired bool
	pins           int
	ctx            context.Context
	cancel         context.CancelFunc
	finish         func()
	timer          *time.Timer
}

func (s *Service) reserveRecipientPackage(owner string) (*recipientPackageEntry, error) {
	s.recipientPackages.mu.Lock()
	defer s.recipientPackages.mu.Unlock()
	if err := s.expireRecipientPackageLocked(); err != nil {
		return nil, err
	}
	if s.recipientPackages.entry != nil {
		return nil, ErrRecipientPackageBusy
	}
	ctx, finish, err := s.beginRun(s.lifecycle)
	if err != nil {
		return nil, ErrRedactionPreparationUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	var id [24]byte
	_, _ = rand.Read(id[:])
	e := &recipientPackageEntry{owner: owner, ctx: ctx, cancel: cancel, finish: finish, pins: 1,
		result: PreparedRecipientPackage{ID: hex.EncodeToString(id[:])}}
	s.recipientPackages.entry = e
	return e, nil
}

// unpinRecipientPackageLocked drops one creation or reader pin and wakes waiters.
func (s *Service) unpinRecipientPackageLocked(e *recipientPackageEntry) error {
	e.pins--
	if s.recipientPackages.released != nil {
		close(s.recipientPackages.released)
		s.recipientPackages.released = nil
	}
	return s.cleanRecipientPackageLocked()
}

func (s *Service) cleanRecipientPackageLocked() error {
	e := s.recipientPackages.entry
	if e == nil || !e.retired || e.pins != 0 {
		return nil
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.cancel()
	if e.finish != nil {
		defer e.finish() // Deletion retries must not keep shutdown registered.
		e.finish = nil
	}
	var closeErr error
	if e.file != nil {
		closeErr = e.file.Close()
		e.file = nil
	}
	if e.directory != "" {
		if err := os.RemoveAll(e.directory); err != nil {
			return errors.Join(closeErr, err)
		}
	}
	s.recipientPackages.entry = nil
	return closeErr
}

func (s *Service) expireRecipientPackageLocked() error {
	e := s.recipientPackages.entry
	if e != nil && e.ready && !s.clock().Before(e.result.ExpiresAt) {
		e.retired = true
	}
	return s.cleanRecipientPackageLocked()
}

// DiscardRecipientPackages cancels only this owner's creation and transfers. It
// returns after that work has stopped and its files are removed, or ctx ends.
func (s *Service) DiscardRecipientPackages(ctx context.Context, owner string) error {
	if owner == "" {
		return ErrRecipientPackageUnavailable
	}
	return s.discardRecipientPackage(ctx, owner, "")
}

// DiscardRecipientPackage releases only the named entry after ticket failure.
func (s *Service) DiscardRecipientPackage(ctx context.Context, owner, id string) error {
	if id == "" {
		return ErrRecipientPackageUnavailable
	}
	return s.discardRecipientPackage(ctx, owner, id)
}

func (s *Service) discardRecipientPackage(ctx context.Context, owner, id string) error {
	if s == nil {
		return nil
	}
	s.recipientPackages.mu.Lock()
	defer s.recipientPackages.mu.Unlock()
	e := s.recipientPackages.entry
	if e == nil || e.owner != owner || id != "" && e.result.ID != id {
		return nil
	}
	e.retired = true
	e.cancel()
	for s.recipientPackages.entry == e && e.pins != 0 {
		if s.recipientPackages.released == nil {
			s.recipientPackages.released = make(chan struct{})
		}
		released := s.recipientPackages.released
		s.recipientPackages.mu.Unlock()
		select {
		case <-released:
		case <-ctx.Done():
			s.recipientPackages.mu.Lock()
			return fmt.Errorf("recipient package release is still in progress: %w", ctx.Err())
		}
		s.recipientPackages.mu.Lock()
	}
	if s.recipientPackages.entry != e {
		return nil
	}
	return s.cleanRecipientPackageLocked()
}

// RevokeRecipientPackages is best-effort cleanup for owner revocation and stop.
// It never waits: revocation callbacks and shutdown must not block on a transfer.
// Empty owner is reserved for service shutdown, never an HTTP-supplied value.
func (s *Service) RevokeRecipientPackages(owner string) {
	if s == nil {
		return
	}
	s.recipientPackages.mu.Lock()
	defer s.recipientPackages.mu.Unlock()
	e := s.recipientPackages.entry
	if e == nil || owner != "" && e.owner != owner {
		return
	}
	e.retired = true
	e.cancel()
	if err := s.cleanRecipientPackageLocked(); err != nil {
		slog.Error("removing revoked recipient package", "error", err)
	}
}

// RecipientPackageReader pins the verified handle until delivery finishes.
type RecipientPackageReader struct {
	*io.SectionReader

	Package PreparedRecipientPackage
	ctx     context.Context
	close   func() error
}

func (r *RecipientPackageReader) Context() context.Context { return r.ctx }
func (r *RecipientPackageReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.SectionReader.Read(p)
}
func (r *RecipientPackageReader) Close() error { return r.close() }

// AcquireRecipientPackage admits one delivery without reopening a mutable path.
func (s *Service) AcquireRecipientPackage(ctx context.Context, owner, id string) (*RecipientPackageReader, error) {
	if s == nil {
		return nil, ErrRecipientPackageUnavailable
	}
	s.recipientPackages.mu.Lock()
	defer s.recipientPackages.mu.Unlock()
	if err := s.expireRecipientPackageLocked(); err != nil {
		return nil, err
	}
	e := s.recipientPackages.entry
	if e == nil || e.owner != owner || e.result.ID != id || !e.ready || e.retired || e.ctx.Err() != nil {
		return nil, ErrRecipientPackageUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	e.pins++
	e.retired = true // The ticket grants only one transfer.
	e.timer.Stop()
	r := &RecipientPackageReader{SectionReader: io.NewSectionReader(e.file, 0, e.result.Size), Package: e.result, ctx: ctx}
	r.close = sync.OnceValue(func() error {
		stop()
		cancel()
		s.recipientPackages.mu.Lock()
		defer s.recipientPackages.mu.Unlock()
		return s.unpinRecipientPackageLocked(e)
	})
	return r, nil
}
