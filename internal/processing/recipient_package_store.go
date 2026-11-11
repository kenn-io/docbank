package processing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// DiscardRecipientPackages cancels only this owner's creation and transfers.
// Empty owner is reserved for service shutdown, never an HTTP-supplied value.
func (s *Service) DiscardRecipientPackages(owner string) error {
	if s == nil {
		return nil
	}
	s.recipientPackages.mu.Lock()
	defer s.recipientPackages.mu.Unlock()
	e := s.recipientPackages.entry
	if e == nil || owner != "" && e.owner != owner {
		return nil
	}
	e.retired = true
	e.cancel()
	return s.cleanRecipientPackageLocked()
}

// RevokeRecipientPackages is best-effort cleanup for owner revocation and stop.
func (s *Service) RevokeRecipientPackages(owner string) {
	if err := s.DiscardRecipientPackages(owner); err != nil {
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
		e.pins--
		return s.cleanRecipientPackageLocked()
	})
	return r, nil
}
