package push

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sync"
	"time"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

var errPublicationIncomplete = errors.New("publication inventory is incomplete")

type publicationPublisher struct {
	mu               sync.Mutex // ScanOnce may observe the next entry while one processor exits.
	connection       *daemonconn.Connection
	opts             Options
	policy           string
	scan             api.NotePublicationScan
	localScan        ingest.FolderScan
	sources          map[string]api.NotePublicationSource
	seen             []api.PublishedNoteIdentity
	report           Report
	status           api.NotePublicationStatus
	current          *api.NotePublicationStatus
	progress         PublicationStatus
	statusErr        error
	bufferedComplete *PublicationStatus
	failed           bool
	completed        bool
	committing       bool
	wait             func(context.Context, time.Duration) error
}

func runPublication(ctx context.Context, c *daemonconn.Connection, opts Options) (Report, error) {
	if c == nil {
		return Report{}, errors.New("publication requires a daemon connection")
	}
	cfg, policy, err := publicationPolicy(opts.Folder)
	if err != nil {
		return Report{}, err
	}
	opts.Folder = cfg
	p := &publicationPublisher{connection: c, opts: opts, policy: policy}
	scanner, err := ingest.NewFolderScanner(cfg, p.process, p.reject)
	if err != nil {
		return Report{}, err
	}
	scanner.VerifyInterval = 60 * time.Second
	scanner.ScanStatus = p.scanStatus
	scanner.BeforeScan = p.beforeScan
	scanner.ObservePresent = p.observePresent
	scanner.AfterScan = p.afterScan
	scanner.BeforeFile = func(_ context.Context, ref string) error {
		if !isPublishedMarkdown(ref) {
			return filepath.SkipDir
		}
		return nil
	}
	if opts.Watch {
		err = scanner.Run(ctx)
	} else {
		err = scanner.ScanOnce(ctx, 1)
	}
	if p.report.Failed > 0 {
		err = errors.Join(err, fmt.Errorf("%w: %d failed", ErrFilesFailed, p.report.Failed))
	}
	return p.report, err
}
func (p *publicationPublisher) beforeScan(ctx context.Context, scan ingest.FolderScan) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bufferedComplete != nil && p.opts.Status != nil {
		if err := p.opts.Status(ctx, clonePublicationStatus(*p.bufferedComplete)); err != nil {
			return err
		}
	}
	p.bufferedComplete = nil
	p.localScan = scan
	p.progress = PublicationStatus{}
	p.statusErr = nil
	p.scan = api.NotePublicationScan{}
	p.failed = false
	p.seen = nil
	request := api.NotePublicationBeginRequest{Name: p.opts.Folder.Name, Destination: p.opts.Folder.Destination, PolicyFingerprint: p.policy, OperationID: uuid.NewV4().String()}
	var remote api.NotePublicationScan
	if err := p.retry(ctx, func() error {
		var err error
		remote, err = p.connection.BeginNotePublicationScan(ctx, request)
		return err
	}, nil); err != nil {
		return err
	}
	p.completed = false
	p.scan = remote
	if err := p.refreshSources(ctx); err != nil {
		return err
	}
	var status api.NotePublicationStatus
	if err := p.retry(ctx, func() error {
		var err error
		status, err = p.connection.NotePublicationStatus(ctx, p.opts.Folder.Name)
		return err
	}, nil); err != nil {
		return err
	}
	if status.DatabaseEpoch != p.scan.DatabaseEpoch {
		return errors.New("publication status changed database epoch")
	}
	p.current = &status
	return p.emit(ctx, "begin")
}
func (p *publicationPublisher) refreshSources(ctx context.Context) error {
	sources := make(map[string]api.NotePublicationSource)
	after := ""
	for {
		var page api.NotePublicationSourcesPage
		if err := p.retry(ctx, func() error {
			var err error
			page, err = p.connection.NotePublicationSources(ctx, api.NotePublicationSourcesRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, After: after, Limit: 1000})
			return err
		}, nil); err != nil {
			return err
		}
		if page.Scan != p.scan {
			return errors.New("publication cursor changed scan identity")
		}
		for _, v := range page.Items {
			sources[v.Identity.SourceRef] = v
		}
		if page.Next == "" {
			p.sources = sources
			return nil
		}
		after = page.Next
	}
}
func (p *publicationPublisher) observePresent(ctx context.Context, v ingest.FolderPresence) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progress.Observed++
	if err := p.emit(ctx, "observed"); err != nil {
		return false, err
	}
	source, known := p.sources[v.Ref]
	if !known || !v.Processed || source.Recovery != nil || source.Conflict != "" || source.Availability != "retained" || v.Digest == "" || v.Digest != source.Identity.Hash || v.Info.Size() != source.Identity.Size || p.localScan.FullVerification {
		return true, nil
	}
	if !v.Stable {
		return false, nil
	}
	return false, p.queueSeen(ctx, source.Identity)
}
func (p *publicationPublisher) queueSeen(ctx context.Context, id api.PublishedNoteIdentity) error {
	request := api.NotePublicationSeenRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, Bindings: append(append([]api.PublishedNoteIdentity(nil), p.seen...), id)}
	fits, err := publicationSeenFits(request)
	if err != nil {
		return err
	}
	if len(request.Bindings) > 200 || !fits {
		if err := p.flushSeen(ctx); err != nil {
			return err
		}
		request.Bindings = []api.PublishedNoteIdentity{id}
		fits, err = publicationSeenFits(request)
		if err != nil {
			return err
		}
		if !fits {
			return errors.New("publication binding exceeds seen body limit")
		}
	}
	p.seen = request.Bindings
	if len(p.seen) == 200 {
		return p.flushSeen(ctx)
	}
	return nil
}
func (p *publicationPublisher) flushSeen(ctx context.Context) error {
	if len(p.seen) == 0 {
		return nil
	}
	var ack api.NotePublicationAck
	err := p.retry(ctx, func() error {
		var err error
		ack, err = p.connection.AcknowledgePublishedNotes(ctx, api.NotePublicationSeenRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, Bindings: p.seen})
		return err
	}, func(_ error) error {
		if err := p.refreshSources(ctx); err != nil {
			return err
		}
		for i, old := range p.seen {
			current, ok := p.sources[old.SourceRef]
			if !ok || current.Conflict != "" || current.Availability != "retained" || !samePublicationContent(current.Identity, old) {
				return errPublicationBindingChanged
			}
			p.seen[i] = current.Identity
		}
		return nil
	})
	if err != nil {
		p.failed = true
		return err
	}
	if ack.Scan.DatabaseEpoch != p.scan.DatabaseEpoch || ack.Scan.Epoch != p.scan.Epoch || ack.Scan.PolicyFingerprint != p.policy {
		p.failed = true
		return errors.New("publication acknowledgement changed scan identity")
	}
	for _, id := range ack.Sources {
		source := p.sources[id.SourceRef]
		source.Identity = id
		source.Presence = "present"
		p.sources[id.SourceRef] = source
		p.report.Skipped++
		p.progress.Unchanged++
		if err := p.emit(ctx, "unchanged"); err != nil {
			return err
		}
		if p.opts.Progress != nil {
			if err := p.opts.Progress(id.SourceRef, "skipped"); err != nil {
				p.failed = true
				return err
			}
		}
	}
	p.seen = nil
	return nil
}
func (p *publicationPublisher) process(ctx context.Context, file ingest.FolderFile) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.publish(ctx, file)
	if err == nil {
		return nil
	}
	if p.statusErr != nil {
		return p.statusErr
	}
	p.failed = true
	code, _ := daemonconn.ProblemCode(err)
	if source, ok := errors.AsType[*publicationSourceError](err); ok {
		code = source.Code
	}
	if ctx.Err() == nil && !publicationStopsRun(err) && (code == "published_source_diverged" || code == "note_source_unavailable") {
		return p.rejectLocked(ctx, file.Ref, err)
	}
	if _, local := errors.AsType[*ingest.SourceAccessError](err); local {
		return err
	}
	p.recordFailure(file.Ref, err)
	if callbackErr := p.emit(ctx, "incomplete"); callbackErr != nil {
		return errors.Join(err, callbackErr)
	}
	return err
}
func (p *publicationPublisher) publish(ctx context.Context, file ingest.FolderFile) error {
	if file.Info.Size() > blob.MaxIngestBytes {
		return fmt.Errorf("source exceeds upload limit of %d bytes", blob.MaxIngestBytes)
	}
	name, err := store.NormalizeName(path.Base(file.Ref))
	if err != nil {
		return err
	}
	digest, err := file.Digest(ctx)
	if err != nil {
		return err
	}
	source, known := p.sources[file.Ref]
	if known {
		if source.Availability == "unavailable" {
			return &publicationSourceError{Code: "note_source_unavailable"}
		}
		if source.Conflict != "" && source.Recovery == nil {
			return &publicationSourceError{Code: "published_source_diverged"}
		}
	}
	if known && source.Recovery == nil && source.Availability == "retained" && source.Identity.Hash == digest && source.Identity.Size == file.Info.Size() {
		return p.queueSeen(ctx, source.Identity)
	}
	if _, err := file.File.Seek(0, io.SeekStart); err != nil {
		return &ingest.SourceAccessError{Err: err}
	}
	revision := int64(0)
	if known {
		revision = source.Identity.SourceRevision
	}
	request := api.NotePublicationUploadRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, SourceRef: file.Ref, FileName: name, ParentPath: path.Join(p.opts.Folder.Destination, path.Dir(file.Ref)), ModifiedAt: file.Info.ModTime().UTC().Format(time.RFC3339Nano), OperationID: uuid.NewV4().String(), Hash: digest, Size: file.Info.Size(), ExpectedSourceRevision: revision}
	var receipt api.NotePublicationReceipt
	err = p.retry(ctx, func() error {
		if _, err := file.File.Seek(0, io.SeekStart); err != nil {
			return &ingest.SourceAccessError{Err: err}
		}
		var err error
		receipt, err = p.connection.UploadPublishedNote(ctx, request, contextReader{ctx: ctx, reader: file.File})
		return err
	}, func(cause error) error {
		current, err := p.recoverUncertain(ctx, file.Ref, request)
		if err != nil {
			return err
		}
		if err := file.Unchanged(); err != nil {
			return err
		}
		if _, err := file.File.Seek(0, io.SeekStart); err != nil {
			return &ingest.SourceAccessError{Err: err}
		}
		now, err := file.Digest(ctx)
		if err != nil {
			return err
		}
		if now != request.Hash {
			return ingest.ErrSourceChanged
		}
		if code, _ := daemonconn.ProblemCode(cause); code == "publication_source_stale" {
			request.ExpectedSourceRevision = current.Identity.SourceRevision
			request.OperationID = uuid.NewV4().String()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if receipt.Scan.DatabaseEpoch != p.scan.DatabaseEpoch || receipt.Scan.Epoch != p.scan.Epoch || receipt.Scan.PolicyFingerprint != p.policy {
		return errors.New("publication upload changed scan identity")
	}
	if current, ok := p.sources[file.Ref]; ok && current.Identity.SourceRevision > receipt.Source.Identity.SourceRevision {
		return errPublicationBindingChanged
	}
	p.sources[file.Ref] = receipt.Source
	switch receipt.Outcome {
	case "added":
		p.report.Added++
	case "updated":
		p.report.Updated++
	case "skipped":
		p.report.Skipped++
	}
	if receipt.Outcome == "skipped" {
		p.progress.Unchanged++
	} else {
		p.progress.Accepted++
	}
	event := "accepted"
	if receipt.Outcome == "skipped" {
		event = "unchanged"
	}
	if err := p.emit(ctx, event); err != nil {
		return err
	}
	if p.opts.Progress != nil {
		return p.opts.Progress(file.Ref, receipt.Outcome)
	}
	return nil
}
func (p *publicationPublisher) reject(ctx context.Context, ref string, err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rejectLocked(ctx, ref, err)
}
func (p *publicationPublisher) rejectLocked(ctx context.Context, ref string, err error) error {
	p.failed = true
	p.report.Failed++
	p.progress.Failed++
	p.recordFailure(ref, err)
	event := "incomplete"
	if publicationFailureCode(err) == "published_source_diverged" {
		event = "conflict"
	}
	if callbackErr := p.emit(ctx, event); callbackErr != nil {
		return callbackErr
	}
	if p.opts.Failure != nil {
		return p.opts.Failure(ref, err)
	}
	return nil
}
func (p *publicationPublisher) afterScan(ctx context.Context, result ingest.FolderScanResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.statusErr != nil {
		return p.statusErr
	}
	p.progress.Failed = max(p.progress.Failed, result.Failed)
	// Verified positive observations remain valid even when another source
	// prevents this inventory from authorizing absence.
	if err := p.flushSeen(ctx); err != nil {
		return err
	}
	if !result.Complete || p.failed {
		if err := p.emit(ctx, "incomplete"); err != nil {
			return err
		}
		if p.opts.Watch {
			return nil
		}
		return errPublicationIncomplete
	}
	if err := p.emit(ctx, "completing"); err != nil {
		return err
	}
	// Once sent, Complete may already be committed even without a receipt.
	// Resolve it without running another fallible authority gate.
	p.committing = true
	defer func() { p.committing = false }()
	var status api.NotePublicationStatus
	request := api.NotePublicationCompleteRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, PolicyFingerprint: p.policy, InventoryComplete: true}
	err := p.retry(ctx, func() error {
		var err error
		status, err = p.connection.CompleteNotePublicationScan(ctx, request)
		return err
	}, func(error) error { return p.refreshSources(ctx) })
	if err != nil {
		return err
	}
	if status.DatabaseEpoch != p.scan.DatabaseEpoch || status.Incomplete {
		return errors.New("publication completion does not confirm the inventory")
	}
	p.status = status
	p.completed = true
	p.current = &status
	p.progress.Scan, p.progress.Event, p.progress.Current = p.scan, "complete", p.current
	final := clonePublicationStatus(p.progress)
	p.report.Publication = &final
	buffered := clonePublicationStatus(final)
	p.bufferedComplete = &buffered
	return nil
}

var errPublicationSeenTooLarge = errors.New("publication seen batch exceeds 1 MiB")

type publicationBodyBudget struct{ remaining int }

func (w *publicationBodyBudget) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errPublicationSeenTooLarge
	}
	w.remaining -= len(b)
	return len(b), nil
}
func publicationSeenFits(r api.NotePublicationSeenRequest) (bool, error) {
	budget := publicationBodyBudget{remaining: 1 << 20}
	err := json.MarshalWrite(&budget, r)
	if errors.Is(err, errPublicationSeenTooLarge) {
		return false, nil
	}
	return err == nil, err
}

var errPublicationBindingChanged = errors.New("publication source binding changed during reconciliation")

func samePublicationContent(a, b api.PublishedNoteIdentity) bool {
	a.NamespaceRevision, b.NamespaceRevision = 0, 0
	a.SourceRevision, b.SourceRevision = 0, 0
	return a == b
}
func publicationStopsRun(err error) bool {
	if stopsRun(err) || errors.Is(err, daemonconn.ErrNotePublicationUnavailable) {
		return true
	}
	code, _ := daemonconn.ProblemCode(err)
	switch code {
	case "publication_scan_superseded", "publication_scan_sealed", "publication_operation_mismatch":
		return true
	}
	return false
}
func publicationRetryable(err error) bool {
	code, _ := daemonconn.ProblemCode(err)
	return !publicationStopsRun(err) && (retryable(err) || code == "publication_source_stale")
}
func publicationWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retry owns one active operation. A retry never starts another traversal or
// creates another Begin; only a proved reconnect callback may replace its socket.
func (p *publicationPublisher) retry(ctx context.Context, action func() error, beforeReplay func(error) error) error {
	return p.retryOperation(ctx, action, beforeReplay, true)
}
func (p *publicationPublisher) retryOperation(ctx context.Context, action func() error, beforeReplay func(error) error, allowReconnect bool) error {
	wait := p.wait
	if wait == nil {
		wait = publicationWait
	}
	for recoveries := 0; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := action()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !publicationRetryable(err) {
			return err
		}
		p.recordFailure("", err)
		if callbackErr := p.emit(ctx, "retry"); callbackErr != nil {
			return callbackErr
		}
		if recoveries == 3 {
			if !p.opts.Watch {
				return err
			}
			if err := wait(ctx, p.opts.Folder.ScanInterval.Std()); err != nil {
				return err
			}
			// The attempt after waiting starts a fresh burst.
			recoveries = -1
		}
		recoveries++
		if allowReconnect && daemonconn.IsTransportError(err) && p.opts.Reconnect != nil {
			var connection *daemonconn.Connection
			reconnectErr := p.retryOperation(ctx, func() error {
				var err error
				connection, err = p.opts.Reconnect(ctx)
				if err == nil && connection == nil {
					return errors.New("publication reconnect returned no proved connection")
				}
				return err
			}, nil, false)
			if reconnectErr != nil {
				return errors.Join(err, reconnectErr)
			}
			p.connection = connection
		}
		if beforeReplay != nil {
			if err := beforeReplay(err); err != nil {
				return err
			}
		}
	}
}
func (p *publicationPublisher) recoverUncertain(ctx context.Context, ref string, _ api.NotePublicationUploadRequest) (api.NotePublicationSource, error) {
	if err := p.refreshSources(ctx); err != nil {
		return api.NotePublicationSource{}, err
	}
	source := p.sources[ref]
	if source.Availability == "unavailable" {
		return source, &publicationSourceError{Code: "note_source_unavailable"}
	}
	if source.Conflict != "" && source.Recovery == nil {
		return source, &publicationSourceError{Code: "published_source_diverged"}
	}
	return source, nil
}

type publicationSourceError struct{ Code string }

func (e *publicationSourceError) Error() string { return e.Code }
