package processing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

type redactionPreviewStore struct {
	mu    sync.Mutex
	entry *redactionPreviewEntry
}

type redactionPreviewEntry struct {
	owner                  string
	manifest               RedactionPreview
	directory, archivePath string
	pagePaths              []string
	textOffset             int64
	ready, retired         bool
	pins                   int
	ctx                    context.Context
	cancel                 context.CancelFunc
	finish                 func()
	timer                  *time.Timer
}

// Called under the cache lock. Capacity remains occupied if removal fails.
func (s *Service) cleanRedactionPreviewLocked() error {
	e := s.redactionPreviews.entry
	if e == nil || !e.retired || e.pins != 0 {
		return nil
	}
	if e.directory != "" {
		if err := os.RemoveAll(e.directory); err != nil {
			return err
		}
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.cancel()
	e.finish()
	s.redactionPreviews.entry = nil
	return nil
}

func (s *Service) expireRedactionPreviewLocked() error {
	e := s.redactionPreviews.entry
	if e != nil && e.ready && !s.clock().Before(e.manifest.ExpiresAt) {
		e.retired = true
	}
	return s.cleanRedactionPreviewLocked()
}

func (s *Service) previewLocked(owner, id string) (*redactionPreviewEntry, error) {
	if err := s.expireRedactionPreviewLocked(); err != nil {
		return nil, err
	}
	e := s.redactionPreviews.entry
	if e == nil || e.owner != owner || e.manifest.ID != id || !e.ready || e.retired || e.ctx.Err() != nil {
		return nil, ErrRedactionPreviewUnavailable
	}
	return e, nil
}

func (s *Service) RedactionPreview(owner, id string) (RedactionPreview, error) {
	if s == nil {
		return RedactionPreview{}, ErrRedactionPreviewUnavailable
	}
	s.redactionPreviews.mu.Lock()
	defer s.redactionPreviews.mu.Unlock()
	e, err := s.previewLocked(owner, id)
	if err != nil {
		return RedactionPreview{}, err
	}
	result := e.manifest
	result.Pages = slices.Clone(result.Pages)
	return result, nil
}

func (s *Service) DiscardRedactionPreview(owner, id string) error {
	if s == nil {
		return ErrRedactionPreviewUnavailable
	}
	s.redactionPreviews.mu.Lock()
	defer s.redactionPreviews.mu.Unlock()
	e, err := s.previewLocked(owner, id)
	if err != nil {
		return err
	}
	e.retired = true
	return s.cleanRedactionPreviewLocked()
}

// DiscardRedactionPreviews interrupts admitted readers and in-flight creation, returning
// cleanup errors so explicit callers can retry. Empty owner is reserved for service
// shutdown; HTTP callers never supply an owner.
func (s *Service) DiscardRedactionPreviews(owner string) error {
	if s == nil {
		return nil
	}
	s.redactionPreviews.mu.Lock()
	defer s.redactionPreviews.mu.Unlock()
	e := s.redactionPreviews.entry
	if e == nil || owner != "" && e.owner != owner {
		return nil
	}
	e.retired = true
	e.cancel()
	return s.cleanRedactionPreviewLocked()
}

// RevokeRedactionPreviews performs best-effort cleanup for logout and shutdown.
func (s *Service) RevokeRedactionPreviews(owner string) {
	if err := s.DiscardRedactionPreviews(owner); err != nil {
		slog.Error("removing revoked redaction preview", "error", err)
	}
}

type RedactionPreviewReader struct {
	*io.SectionReader

	Digest   RedactionFileDigest
	Manifest RedactionPreview
	ctx      context.Context
	close    func() error
}

func (r *RedactionPreviewReader) Context() context.Context { return r.ctx }
func (r *RedactionPreviewReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.SectionReader.Read(p)
}
func (r *RedactionPreviewReader) Close() error { return r.close() }

// AcquireRedactionPreview pins the directory before opening a file, under the same lock as
// retirement. Each reader has its own file and offset.
func (s *Service) AcquireRedactionPreview(ctx context.Context, owner, id, part string, page int) (*RedactionPreviewReader, error) {
	if s == nil {
		return nil, ErrRedactionPreviewUnavailable
	}
	s.redactionPreviews.mu.Lock()
	defer s.redactionPreviews.mu.Unlock()
	e, err := s.previewLocked(owner, id)
	if err != nil {
		return nil, err
	}
	path, offset, digest := e.archivePath, int64(0), RedactionFileDigest{Size: e.manifest.Archive.Size, SHA256: e.manifest.Archive.SHA256}
	switch part {
	case "bundle":
	case "text":
		offset, digest = e.textOffset, e.manifest.Archive.Receipt.Text
	case "page":
		if page < 1 || page > len(e.pagePaths) {
			return nil, ErrRedactionPreviewUnavailable
		}
		path, digest = e.pagePaths[page-1], e.manifest.Pages[page-1].PNG
	default:
		return nil, ErrRedactionInvalidRequest
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	e.pins++
	r := &RedactionPreviewReader{SectionReader: io.NewSectionReader(file, offset, digest.Size), Digest: digest,
		Manifest: e.manifest, ctx: readCtx}
	r.close = sync.OnceValue(func() error {
		stop()
		cancel()
		err := file.Close()
		s.redactionPreviews.mu.Lock()
		defer s.redactionPreviews.mu.Unlock()
		e.pins--
		return errors.Join(err, s.cleanRedactionPreviewLocked())
	})
	return r, nil
}
