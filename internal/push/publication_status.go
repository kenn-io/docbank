package push

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

// PublicationStatus separates this scan's observations from cumulative uploads.
// Current is one server snapshot with its own freshness, never a sum of pages.
// Callbacks run before commit; the confirmed complete snapshot is returned in
// Report.Publication. Successful byte upload does not imply indexing readiness.
type PublicationStatus struct {
	Scan                                  api.NotePublicationScan
	Event                                 string
	Observed, Accepted, Unchanged, Failed int64
	Degraded                              bool
	LastVerifiedAt, VerificationDueAt     time.Time
	VerificationLag                       time.Duration
	Current                               *api.NotePublicationStatus
	Errors                                []PublicationFailure
	ErrorsOmitted                         int64
}

// PublicationFailure deliberately excludes raw errors, bodies and local paths.
type PublicationFailure struct{ Code, SourceRef string }

func clonePublicationStatus(s PublicationStatus) PublicationStatus {
	s.Errors = append([]PublicationFailure(nil), s.Errors...)
	if s.Current != nil {
		current := *s.Current
		if current.ActiveScan != nil {
			scan := *current.ActiveScan
			current.ActiveScan = &scan
		}
		s.Current = &current
	}
	return s
}

// emit is called under the publisher lock, including retry/status paths.
func (p *publicationPublisher) emit(ctx context.Context, event string) error {
	p.progress.Scan, p.progress.Event, p.progress.Current = p.scan, event, p.current
	if !p.committing && p.opts.Status != nil {
		if err := p.opts.Status(ctx, clonePublicationStatus(p.progress)); err != nil {
			p.failed = true
			p.statusErr = err
			return err
		}
	}
	return ctx.Err()
}
func (p *publicationPublisher) scanStatus(ctx context.Context, s ingest.FolderScanStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progress.Degraded = s.Degraded
	p.progress.LastVerifiedAt = s.LastVerifiedAt
	p.progress.VerificationDueAt = s.VerificationDueAt
	p.progress.VerificationLag = s.VerificationLag
	return p.emit(ctx, "verification")
}
func publicationFailureCode(err error) string {
	code, _ := daemonconn.ProblemCode(err)
	if source, ok := errors.AsType[*publicationSourceError](err); ok {
		code = source.Code
	}
	switch code {
	case "published_source_diverged", "note_source_unavailable", "publication_source_stale", "publication_scan_superseded", "publication_scan_sealed", "publication_operation_mismatch", "digest_mismatch", "size_mismatch":
		return code
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, daemonconn.ErrNotePublicationUnavailable):
		return "capability_unavailable"
	case daemonconn.IsTransportError(err), errors.Is(err, daemonconn.ErrPushResponseInterrupted):
		return "transport"
	case errors.Is(err, ingest.ErrSourceChanged):
		return "source_changed"
	default:
		return "source_access"
	}
}
func (p *publicationPublisher) recordFailure(ref string, err error) {
	// This is a bounded sample. Counts include all failures, not only examples.
	if len(p.progress.Errors) == 8 {
		p.progress.ErrorsOmitted++
		return
	}
	if store.ValidatePushRef(ref) != nil {
		ref = ""
	}
	p.progress.Errors = append(p.progress.Errors, PublicationFailure{Code: publicationFailureCode(err), SourceRef: ref})
}
