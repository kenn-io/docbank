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
)

var errPublicationIncomplete = errors.New("publication inventory is incomplete")

type publicationPublisher struct {
	mu         sync.Mutex // ScanOnce may observe the next entry while one processor exits.
	connection *daemonconn.Connection
	opts       Options
	policy     string
	scan       api.NotePublicationScan
	localScan  ingest.FolderScan
	sources    map[string]api.NotePublicationSource
	seen       []api.PublishedNoteIdentity
	report     Report
	status     api.NotePublicationStatus
	failed     bool
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
	p.localScan = scan
	p.failed = false
	p.seen = nil
	remote, err := p.connection.BeginNotePublicationScan(ctx, api.NotePublicationBeginRequest{Name: p.opts.Folder.Name, Destination: p.opts.Folder.Destination, PolicyFingerprint: p.policy, OperationID: uuid.NewV4().String()})
	if err != nil {
		return err
	}
	p.scan = remote
	return p.refreshSources(ctx)
}
func (p *publicationPublisher) refreshSources(ctx context.Context) error {
	sources := make(map[string]api.NotePublicationSource)
	after := ""
	for {
		page, err := p.connection.NotePublicationSources(ctx, api.NotePublicationSourcesRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, After: after, Limit: 1000})
		if err != nil {
			return err
		}
		if page.Scan.ScanID != p.scan.ScanID || page.Scan.DatabaseEpoch != p.scan.DatabaseEpoch || page.Scan.Epoch != p.scan.Epoch || page.Scan.PolicyFingerprint != p.policy {
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
	source, known := p.sources[v.Ref]
	if !known || source.Recovery != nil || source.Conflict != "" || source.Availability != "retained" || v.Digest == "" || v.Digest != source.Identity.Hash || v.Info.Size() != source.Identity.Size || p.localScan.FullVerification {
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
	ack, err := p.connection.AcknowledgePublishedNotes(ctx, api.NotePublicationSeenRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, Bindings: p.seen})
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
	if err != nil {
		p.failed = true
	}
	return err
}
func (p *publicationPublisher) publish(ctx context.Context, file ingest.FolderFile) error {
	if file.Info.Size() > blob.MaxIngestBytes {
		return fmt.Errorf("source exceeds upload limit of %d bytes", blob.MaxIngestBytes)
	}
	digest, err := file.Digest(ctx)
	if err != nil {
		return err
	}
	source, known := p.sources[file.Ref]
	if known && source.Conflict != "" && source.Recovery == nil {
		return fmt.Errorf("publication source %q conflicts with archive: %s", file.Ref, source.Conflict)
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
	receipt, err := p.connection.UploadPublishedNote(ctx, api.NotePublicationUploadRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, SourceRef: file.Ref, FileName: path.Base(file.Ref), ParentPath: path.Join(p.opts.Folder.Destination, path.Dir(file.Ref)), ModifiedAt: file.Info.ModTime().UTC().Format(time.RFC3339Nano), OperationID: uuid.NewV4().String(), Hash: digest, Size: file.Info.Size(), ExpectedSourceRevision: revision}, contextReader{ctx: ctx, reader: file.File})
	if err != nil {
		return err
	}
	if receipt.Scan.DatabaseEpoch != p.scan.DatabaseEpoch || receipt.Scan.Epoch != p.scan.Epoch || receipt.Scan.PolicyFingerprint != p.policy {
		return errors.New("publication upload changed scan identity")
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
	if p.opts.Progress != nil {
		return p.opts.Progress(file.Ref, receipt.Outcome)
	}
	return nil
}
func (p *publicationPublisher) reject(_ context.Context, ref string, err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed = true
	p.report.Failed++
	if p.opts.Failure != nil {
		return p.opts.Failure(ref, err)
	}
	return nil
}
func (p *publicationPublisher) afterScan(ctx context.Context, result ingest.FolderScanResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !result.Complete || p.failed {
		return errPublicationIncomplete
	}
	if err := p.flushSeen(ctx); err != nil {
		return err
	}
	status, err := p.connection.CompleteNotePublicationScan(ctx, api.NotePublicationCompleteRequest{Name: p.opts.Folder.Name, ScanID: p.scan.ScanID, PolicyFingerprint: p.policy, InventoryComplete: true})
	if err != nil {
		return err
	}
	if status.DatabaseEpoch != p.scan.DatabaseEpoch || status.Incomplete {
		return errors.New("publication completion does not confirm the inventory")
	}
	p.status = status
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
