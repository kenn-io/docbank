package mailbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/store"
)

func (s *Service) sourceEntries(ctx context.Context, c store.MailboxContainer) ([]Entry, *ChunkReaderAt, error) {
	r, err := s.ReaderAt(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	if c.Format == "zip" {
		_, entries, err := zipDirectory(r, c.Size, DefaultArchiveLimits())
		return entries, r, err
	}
	if c.Format != "mbox" || c.Size > 50<<30 {
		return nil, nil, store.ErrMailboxLimit
	}
	return []Entry{{Index: 0, Name: "source.mbox", SHA256: c.SHA256, Size: c.Size}}, r, nil
}
func openEntry(ctx context.Context, e Entry, r *ChunkReaderAt, offset int64) (io.ReadCloser, error) {
	if offset < 0 || offset > e.Size {
		return nil, store.ErrMailboxConflict
	}
	if e.File != nil {
		stream, err := e.File.Open()
		if err != nil {
			return nil, fmt.Errorf("open mailbox entry: %w", err)
		}
		// Deflate has no seek primitive. Discard the validated prefix without
		// rescanning or spooling already committed messages.
		if _, err = io.CopyN(io.Discard, contextReader{ctx, stream}, offset); err != nil {
			return nil, errors.Join(err, stream.Close())
		}
		return stream, nil
	}
	return io.NopCloser(contextReader{ctx, io.NewSectionReader(r, offset, e.Size-offset)}), nil
}
func (s *Service) RunJob(ctx context.Context, j store.MailboxJob) error {
	return s.runJob(ctx, j, store.MailboxSegmentMessages)
}
func (s *Service) runJob(ctx context.Context, j store.MailboxJob, segmentMessages int64) (retErr error) {
	if segmentMessages < 1 || segmentMessages > store.MailboxSegmentMessages {
		return store.ErrMailboxLimit
	}
	defer func() {
		if retErr != nil && ctx.Err() == nil {
			retErr = errors.Join(retErr, s.failJob(ctx, j, retErr))
		}
	}()
	c, err := s.Store.MailboxContainer(ctx, j.Owner, j.ContainerID)
	if err != nil {
		return err
	}
	if c.SHA256 != j.ContainerSHA256 {
		return store.ErrMailboxConflict
	}
	if err = j.Settings.ValidateExecution(); err != nil {
		return err
	}
	entries, reader, err := s.sourceEntries(ctx, c)
	if err != nil {
		return err
	}
	if c.Format == "zip" {
		if len(j.EntryHashes) == 0 {
			entries, err = PreflightZIP(ctx, reader, c.Size, DefaultArchiveLimits())
			if err != nil {
				return err
			}
			for _, entry := range entries {
				j.EntryHashes = append(j.EntryHashes, entry.SHA256)
			}
			if err = s.mutate(ctx, func() error { return s.Store.VerifyMailboxJobEntries(ctx, j.ID, j.Claim, j.EntryHashes) }); err != nil {
				return err
			}
		} else {
			if len(j.EntryHashes) != len(entries) {
				return store.ErrMailboxConflict
			}
			for i := range entries {
				entries[i].SHA256 = j.EntryHashes[i]
			}
		}
	}
	checkpoint, err := s.resumeCheckpoint(ctx, j, c, entries)
	if err != nil {
		return err
	}
	archive := "mailbox:" + j.CollectionID
	if err = s.mutate(ctx, func() error {
		return s.Store.RegisterMailboxArchive(ctx, store.MailboxArchive{ID: archive, Owner: j.Owner, Description: "Explicit mailbox archive import"})
	}); err != nil {
		return err
	}
	settings, err := j.Settings.Canonical()
	if err != nil {
		return err
	}
	ordinal := j.Checkpoint
	for _, entry := range entries {
		var offset, sequence int64
		if checkpoint.Sequence > 0 {
			if entry.Index < checkpoint.EntryIndex {
				continue
			}
			if entry.Index == checkpoint.EntryIndex {
				offset, sequence = checkpoint.End, checkpoint.Sequence
			}
		}
		if offset == entry.Size {
			continue
		}
		stream, err := openEntry(ctx, entry, reader, offset)
		if err != nil {
			return err
		}
		scan, err := NewScanner(contextReader{ctx, stream}, j.Settings.Dialect, s.Spool, 128<<20)
		if err != nil {
			return errors.Join(err, stream.Close())
		}
		scan.offset, scan.sequence = offset, sequence
		finished, scanErr := func() (bool, error) {
			for {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				current, err := s.Store.MailboxJob(ctx, j.Owner, j.ID)
				if err != nil {
					return false, err
				}
				if current.State != "running" || current.Claim != j.Claim {
					return false, store.ErrMailboxConflict
				}
				// The unscanned tail remains explicit. Continuation may discover EOF.
				if current.Checkpoint-j.SegmentStart >= segmentMessages {
					return true, s.finishJob(ctx, j, "partial", "Segment limit reached; continue this immutable source to scan the remaining tail.", false)
				}
				m, err := scan.Next(ctx)
				if errors.Is(err, io.EOF) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				ordinal++
				if err = s.importOccurrence(ctx, j, entry, m, ordinal, settings); err != nil {
					return false, err
				}
			}
		}()
		if err = errors.Join(scanErr, stream.Close()); err != nil {
			return err
		}
		if finished {
			return nil
		}
	}
	current, err := s.Store.MailboxJob(ctx, j.Owner, j.ID)
	if err != nil {
		return err
	}
	state := "complete"
	reason := ""
	if current.Rejected > 0 {
		state = "partial"
		reason = "All occurrences scanned; some messages were rejected."
	}
	return s.finishJob(ctx, j, state, reason, true)
}

func (s *Service) importOccurrence(ctx context.Context, j store.MailboxJob, entry Entry, m *Message, ordinal int64, settings string) (retErr error) {
	defer func() { retErr = errors.Join(retErr, m.Close()) }()
	location := store.MailboxLocation{ContainerID: j.ContainerID, Entry: entry.Name, EntryIndex: entry.Index, EntrySHA256: entry.SHA256, Sequence: m.Sequence, Start: m.Start, End: m.End, Separator: m.Separator, RawSHA256: m.RawSHA256, EMLSHA256: m.EMLSHA256, EMLSize: m.EMLSize, Labels: []string{}}
	o := store.MailboxOccurrence{JobID: j.ID, Ordinal: ordinal, Location: location, Outcome: "rejected", Reason: m.Rejection}
	if err := s.mutate(ctx, func() error { return s.Store.StartMailboxOccurrence(ctx, j.ID, j.Claim, o) }); err != nil {
		return err
	}
	if m.Rejection == "" {
		request := store.MailboxTransferRequest{ArchiveID: "mailbox:" + j.CollectionID, Reference: fmt.Sprintf("%d:%d", entry.Index, m.Sequence), SHA256: m.EMLSHA256, Size: m.EMLSize, Settings: settings, DestinationID: j.Settings.DestinationID, Name: "message.eml"}
		_, err := s.transferSource(ctx, j.Owner, j.IngestRun(), request, m.EML, &location, &occurrenceCommit{job: j, occurrence: o})
		if !errors.Is(err, ErrRejected) {
			return err
		}
		o.Reason = err.Error()
	}
	return s.mutate(ctx, func() error { return s.Store.CommitMailboxOccurrence(ctx, j.ID, j.Claim, o, nil) })
}

func (s *Service) resumeCheckpoint(ctx context.Context, j store.MailboxJob, c store.MailboxContainer, entries []Entry) (store.MailboxLocation, error) {
	if j.Checkpoint == 0 {
		return store.MailboxLocation{}, nil
	}
	previous, err := s.Store.MailboxOccurrences(ctx, j.Owner, j.ID, j.Checkpoint-1, 1)
	if err != nil {
		return store.MailboxLocation{}, err
	}
	if len(previous) != 1 || previous[0].Ordinal != j.Checkpoint {
		return store.MailboxLocation{}, store.ErrMailboxConflict
	}
	location := previous[0].Location
	if location.ContainerID != c.ID || location.Sequence < 1 || location.Start < 0 || location.End <= location.Start {
		return store.MailboxLocation{}, store.ErrMailboxConflict
	}
	for _, entry := range entries {
		if location.EntryIndex != entry.Index {
			continue
		}
		if location.Entry != entry.Name || location.EntrySHA256 != entry.SHA256 || location.End > entry.Size {
			return store.MailboxLocation{}, store.ErrMailboxConflict
		}
		return location, nil
	}
	return store.MailboxLocation{}, store.ErrMailboxConflict
}

// RunWorker belongs to the daemon supervisor. At most two workers claim work;
// the catalog enforces both global and owner bounds across all callers.
func (s *Service) RunWorker(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var j store.MailboxJob
		err := s.mutate(ctx, func() error { var err error; j, err = s.Store.ClaimMailboxJob(ctx); return err })
		if err == nil {
			if err = s.runCancelableJob(ctx, j); err != nil && ctx.Err() == nil {
				logger := s.Logger
				if logger == nil {
					logger = slog.Default()
				}
				logger.ErrorContext(ctx, "mailbox job failed", "job", j.ID, "error", err)
			}
			continue
		}
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrMailboxLimit) && !s.Store.RenditionJobErrorRetryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Service) runCancelableJob(ctx context.Context, j store.MailboxJob) error {
	parent := ctx
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(context.Canceled)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := s.Store.MailboxJob(ctx, j.Owner, j.ID)
				if s.Store.RenditionJobErrorRetryable(err) {
					continue
				}
				if err != nil || current.State != "running" || current.Claim != j.Claim {
					if err == nil {
						err = store.ErrMailboxConflict
					}
					cancel(err)
					return
				}
			}
		}
	}()
	err := s.RunJob(ctx, j)
	cause := context.Cause(ctx)
	cancel(context.Canceled)
	<-done
	if err != nil && parent.Err() == nil && cause != nil {
		err = errors.Join(err, cause, s.failJob(parent, j, cause))
	}
	return err
}

func (s *Service) finishJob(ctx context.Context, j store.MailboxJob, state, reason string, tail bool) error {
	return s.mutate(ctx, func() error { return s.Store.FinishMailboxJob(ctx, j.ID, j.Claim, state, reason, tail) })
}

func (s *Service) failJob(ctx context.Context, j store.MailboxJob, cause error) error {
	reason := cause.Error()
	if len(reason) > 4096 {
		reason = "mailbox import failed"
	}
	for {
		state := "failed"
		current, err := s.Store.MailboxJob(ctx, j.Owner, j.ID)
		if err == nil {
			if current.Checkpoint > 0 {
				state = "partial"
			}
			err = s.finishJob(ctx, j, state, reason, false)
		}
		if errors.Is(err, store.ErrMailboxConflict) {
			return nil
		}
		if !s.Store.RenditionJobErrorRetryable(err) {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
