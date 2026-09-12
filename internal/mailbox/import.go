package mailbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/store"
)

func (s *Service) sourceEntries(ctx context.Context, c store.MailboxContainer) ([]Entry, *ChunkReaderAt, error) {
	r, err := s.ReaderAt(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	if c.Format == "zip" {
		entries, err := PreflightZIP(ctx, r, c.Size, DefaultArchiveLimits())
		return entries, r, err
	}
	if c.Format != "mbox" || c.Size > 50<<30 {
		return nil, nil, store.ErrMailboxLimit
	}
	return []Entry{{Index: 0, Name: "source.mbox", SHA256: c.SHA256, Size: c.Size}}, r, nil
}
func openEntry(ctx context.Context, e Entry, r *ChunkReaderAt) (io.ReadCloser, error) {
	if e.File != nil {
		stream, err := e.File.Open()
		if err != nil {
			return nil, fmt.Errorf("open mailbox entry: %w", err)
		}
		return stream, nil
	}
	return io.NopCloser(contextReader{ctx, io.NewSectionReader(r, 0, e.Size)}), nil
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
			reason := retErr.Error()
			if len(reason) > 4096 {
				reason = "mailbox import failed"
			}
			state := "failed"
			if current, err := s.Store.MailboxJob(ctx, j.Owner, j.ID); err == nil && current.Checkpoint > 0 {
				state = "partial"
			}
			finishErr := s.finishJob(ctx, j, state, reason, false)
			if !errors.Is(finishErr, store.ErrMailboxConflict) {
				retErr = errors.Join(retErr, finishErr)
			}
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
	var ordinal int64
	for _, entry := range entries {
		stream, err := openEntry(ctx, entry, reader)
		if err != nil {
			return err
		}
		scan, err := NewScanner(contextReader{ctx, stream}, j.Settings.Dialect, s.Spool, 128<<20)
		if err != nil {
			return errors.Join(err, stream.Close())
		}
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
				if ordinal <= j.Checkpoint {
					if err = m.Close(); err != nil {
						return false, err
					}
					continue
				}
				location := store.MailboxLocation{ContainerID: c.ID, Entry: entry.Name, EntryIndex: entry.Index, EntrySHA256: entry.SHA256, Sequence: m.Sequence, Start: m.Start, End: m.End, Separator: m.Separator, RawSHA256: m.RawSHA256, EMLSHA256: m.EMLSHA256, EMLSize: m.EMLSize, Labels: []string{}}
				o := store.MailboxOccurrence{JobID: j.ID, Ordinal: ordinal, Location: location, Outcome: "rejected", Reason: m.Rejection}
				if err = s.mutate(ctx, func() error { return s.Store.StartMailboxOccurrence(ctx, j.ID, j.Claim, o) }); err != nil {
					return false, errors.Join(err, m.Close())
				}
				if m.Rejection == "" {
					labels, labelErr := messageLabels(m)
					if labelErr != nil {
						err = labelErr
					} else {
						location.Labels = labels
						o.Location = location
						request := store.MailboxTransferRequest{ArchiveID: archive, Reference: fmt.Sprintf("%d:%d", entry.Index, m.Sequence), SHA256: m.EMLSHA256, Size: m.EMLSize, Settings: settings, DestinationID: j.Settings.DestinationID, Name: "message.eml"}
						_, err = s.transfer(ctx, j.Owner, j.IngestRun(), request, m.EML, &location, &occurrenceCommit{job: j, occurrence: o})
					}
					if errors.Is(err, ErrRejected) {
						o.Reason = err.Error()
						err = s.mutate(ctx, func() error { return s.Store.CommitMailboxOccurrence(ctx, j.ID, j.Claim, o, nil) })
					}
				} else {
					err = s.mutate(ctx, func() error { return s.Store.CommitMailboxOccurrence(ctx, j.ID, j.Claim, o, nil) })
				}
				if closeErr := m.Close(); err != nil || closeErr != nil {
					return false, errors.Join(err, closeErr)
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
	if current.Checkpoint == 0 {
		return store.ErrMailboxInvalid
	}
	state := "complete"
	reason := ""
	if current.Rejected > 0 {
		state = "partial"
		reason = "All occurrences scanned; some messages were rejected."
	}
	return s.finishJob(ctx, j, state, reason, true)
}
func messageLabels(m *Message) ([]string, error) {
	if _, err := m.EML.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	headers, err := textproto.NewReader(bufio.NewReader(io.LimitReader(m.EML, 1<<20))).ReadMIMEHeader()
	_, seekErr := m.EML.Seek(0, io.SeekStart)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%w: invalid RFC5322 headers", ErrRejected), seekErr)
	}
	if seekErr != nil {
		return nil, seekErr
	}
	labels := []string{}
	for _, value := range headers.Values("X-Gmail-Labels") {
		for label := range strings.SplitSeq(value, ",") {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			if len(label) > 256 || len(labels) >= 100 {
				return nil, fmt.Errorf("%w: source label limit", ErrRejected)
			}
			labels = append(labels, label)
		}
	}
	return labels, nil
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
			_ = s.runCancelableJob(ctx, j)
			continue
		}
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrMailboxLimit) {
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
				if err != nil || current.State != "running" || current.Claim != j.Claim {
					cancel()
					return
				}
			}
		}
	}()
	err := s.RunJob(ctx, j)
	cancel()
	<-done
	return err
}

func (s *Service) finishJob(ctx context.Context, j store.MailboxJob, state, reason string, tail bool) error {
	return s.mutate(ctx, func() error { return s.Store.FinishMailboxJob(ctx, j.ID, j.Claim, state, reason, tail) })
}
