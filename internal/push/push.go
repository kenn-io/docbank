// Package push streams a read-only local folder to an explicitly selected
// keyed daemon. The server's source cursor is the only resumability authority.
package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

// ErrFilesFailed reports that some files were not pushed. Every other file
// was processed, and running the same command again retries the failed ones.
var ErrFilesFailed = errors.New("some files were not pushed")

// Options defines one folder push. Watch uses the watched-inbox stability
// defaults and rules; a one-shot run processes current files immediately.
// Progress receives each acknowledged outcome and Failure each file the run
// skipped after the daemon or the local source rejected it.
type Options struct {
	Folder   config.WatchConfig
	Watch    bool
	Progress func(ref, outcome string) error
	Failure  func(ref string, err error) error
}

// Report counts only independently acknowledged file outcomes, plus the files
// the run skipped after a rejection. Failed runs return their partial report
// and an error; callers may safely run again.
type Report struct {
	Added   int
	Updated int
	Skipped int
	Failed  int
}

// Run executes a folder push without discovering or starting a local daemon.
// It first reads every cursor of the push name, so a wrong key, a failed key
// proof, or a push name in use by a daemon watch fails before any scan. Watch
// mode survives a retryable failure of that first read and rereads the cursors
// at the start of every scan that has files to push. Each scan is one ingest
// run on the daemon. A file the daemon rejects is reported
// and skipped. Transport failures and daemon-side errors stop a one-shot run;
// watch mode observes the file again after a fresh settle window. A failed key
// proof, an invalid daemon response, a refused key, or a push name in use by a
// daemon watch stops either mode.
func Run(ctx context.Context, connection *daemonconn.Connection, opts Options) (Report, error) {
	var report Report
	if connection == nil {
		return report, errors.New("push requires a daemon connection")
	}
	p := &pusher{connection: connection, opts: opts, report: &report}
	scanner, err := ingest.NewFolderScanner(opts.Folder, p.process, p.reject)
	if err != nil {
		return report, err
	}
	p.cursors, err = connection.PushCursors(ctx, opts.Folder.Name)
	if err != nil && (!opts.Watch || stopsRun(err) || !retryable(err)) {
		return report, err
	}
	if opts.Watch {
		err = scanner.Run(ctx)
	} else {
		err = scanner.ScanOnce(ctx)
	}
	if report.Failed > 0 {
		err = errors.Join(err, fmt.Errorf("%w: %d failed", ErrFilesFailed, report.Failed))
	}
	return report, err
}

// pusher holds a copy of the daemon's cursors and updates it from its own
// receipts. A one-shot run is a single scan, so its startup copy is current.
// Watch mode rereads the cursors at the start of each scan that has files to
// push: an upload whose response was lost, a trashed or purged document, or
// another producer can change the daemon's cursors between scans.
type pusher struct {
	connection *daemonconn.Connection
	opts       Options
	report     *Report
	cursors    map[string]api.PushSourceCursor
	cursorScan uint64
	cursorErr  error
	runScan    uint64
	runID      string
}

// reject reports a file or subdirectory the scanner could not read. The
// scanner reports it once and continues with the rest of the folder.
func (p *pusher) reject(_ context.Context, ref string, err error) error {
	p.report.Failed++
	if p.opts.Failure != nil {
		return p.opts.Failure(ref, err)
	}
	return nil
}

// refreshCursors rereads the daemon's cursors once per watch scan. A failed
// read is remembered for the rest of that scan, so the remaining files are
// deferred without hashing them or repeating the request; a later scan tries
// again.
func (p *pusher) refreshCursors(ctx context.Context, scan uint64) error {
	if scan == p.cursorScan {
		return p.cursorErr
	}
	cursors, err := p.connection.PushCursors(ctx, p.opts.Folder.Name)
	p.cursorScan, p.cursorErr = scan, err
	if err == nil {
		p.cursors = cursors
	}
	return err
}

func (p *pusher) process(ctx context.Context, file ingest.FolderFile) error {
	outcome, err := p.push(ctx, file)
	if err != nil {
		return p.fail(ctx, file, err)
	}
	switch outcome {
	case "added":
		p.report.Added++
	case "updated":
		p.report.Updated++
	case "skipped":
		p.report.Skipped++
	}
	if p.opts.Progress != nil {
		return p.opts.Progress(file.Ref, outcome)
	}
	return nil
}

// fail decides whether one file's error stops the run, asks the scanner to
// observe the file again later, or is reported while the run continues. A
// rejection counts as a failure only when the file still matches what was
// hashed: a digest or size rejection after a local edit is a source change.
func (p *pusher) fail(ctx context.Context, file ingest.FolderFile, err error) error {
	if ctx.Err() != nil || errors.Is(err, ingest.ErrSourceChanged) || stopsRun(err) {
		return err
	}
	if _, local := errors.AsType[*ingest.SourceAccessError](err); local {
		return err
	}
	if retryable(err) {
		return fmt.Errorf("%w: %w", ingest.ErrRetryLater, err)
	}
	if changed := file.Unchanged(); changed != nil {
		if errors.Is(changed, ingest.ErrSourceChanged) {
			return changed
		}
		return fmt.Errorf("%w: %w", ingest.ErrRetryLater, changed)
	}
	p.report.Failed++
	if p.opts.Failure != nil {
		return p.opts.Failure(file.Ref, err)
	}
	return nil
}

func stopsRun(err error) bool {
	if daemonconn.IsKeyProofError(err) || daemonconn.IsResponseDecodeError(err) {
		return true
	}
	if code, ok := daemonconn.ProblemCode(err); ok && code == "push_name_in_use" {
		return true
	}
	status, ok := daemonconn.ResponseStatus(err)
	return ok && (status == http.StatusUnauthorized || status == http.StatusForbidden)
}

func retryable(err error) bool {
	if daemonconn.IsTransportError(err) || errors.Is(err, daemonconn.ErrPushResponseInterrupted) {
		return true
	}
	status, ok := daemonconn.ResponseStatus(err)
	return ok && (status >= http.StatusInternalServerError ||
		status == http.StatusRequestTimeout || status == http.StatusTooManyRequests)
}

func (p *pusher) push(ctx context.Context, file ingest.FolderFile) (string, error) {
	if file.Info.Size() > blob.MaxIngestBytes {
		return "", fmt.Errorf("source exceeds upload limit of %d bytes", blob.MaxIngestBytes)
	}
	if p.opts.Watch {
		if err := p.refreshCursors(ctx, file.Scan); err != nil {
			return "", err
		}
	}
	digest, err := file.Digest(ctx)
	if err != nil {
		return "", err
	}
	cursor, known := p.cursors[file.Ref]
	if known && cursor.Trashed {
		return "", fmt.Errorf("archived document %d is in trash; restore it to push this file", cursor.NodeID)
	}
	if known && cursor.Hash == digest && cursor.Size == file.Info.Size() {
		return "skipped", nil
	}
	mimeType, err := detectMIME(file)
	if err != nil {
		return "", err
	}
	if file.Scan != p.runScan {
		p.runScan, p.runID = file.Scan, uuid.NewV4().String()
	}
	receipt, err := p.connection.PushUpload(ctx, store.PushSource{
		Name: p.opts.Folder.Name, Ref: file.Ref,
		ModifiedAt: file.Info.ModTime().UTC().Format(time.RFC3339Nano),
	}, store.PushContent{
		RunID:      p.runID,
		ParentPath: path.Join(p.opts.Folder.Destination, path.Dir(file.Ref)),
		Name:       path.Base(file.Ref), Hash: digest, Size: file.Info.Size(), MIMEType: mimeType,
	}, contextReader{ctx: ctx, reader: file.File})
	if err != nil {
		if code, ok := daemonconn.ProblemCode(err); ok && (code == "digest_mismatch" || code == "size_mismatch") {
			// Metadata alone cannot detect an in-place edit that restores mtime.
			if _, seekErr := file.File.Seek(0, io.SeekStart); seekErr != nil {
				return "", &ingest.SourceAccessError{Err: fmt.Errorf("rewinding rejected source: %w", seekErr)}
			}
			current, hashErr := file.Digest(ctx)
			if hashErr != nil {
				return "", hashErr
			}
			if current != digest {
				return "", ingest.ErrSourceChanged
			}
		}
		return "", err
	}
	p.cursors[file.Ref] = api.PushSourceCursor{
		SourceRef: file.Ref, NodeID: receipt.Node.ID, Hash: digest, Size: file.Info.Size(),
	}
	return receipt.Status, nil
}

// detectMIME sniffs the file head and leaves the file positioned at its start.
func detectMIME(file ingest.FolderFile) (string, error) {
	if _, err := file.File.Seek(0, io.SeekStart); err != nil {
		return "", &ingest.SourceAccessError{Err: fmt.Errorf("rewinding source: %w", err)}
	}
	head := make([]byte, 512)
	n, err := file.File.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", &ingest.SourceAccessError{Err: fmt.Errorf("reading file type: %w", err)}
	}
	if _, err := file.File.Seek(0, io.SeekStart); err != nil {
		return "", &ingest.SourceAccessError{Err: fmt.Errorf("rewinding source: %w", err)}
	}
	return ingest.DetectMIME(file.Ref, head[:n]), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	if err != nil && !errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = &ingest.SourceAccessError{Err: fmt.Errorf("reading source: %w", err)}
	}
	return n, err
}
