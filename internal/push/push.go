// Package push streams a read-only local folder to an explicitly selected
// keyed daemon. The server's source cursor is the only resumability authority.
package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

// ErrFilesFailed reports that some files were not pushed. Every other file
// was processed, and running the same command again retries the failed ones.
var ErrFilesFailed = errors.New("some files were not pushed")

var errInvalidCursor = errors.New("daemon returned an invalid push source cursor")

// Options defines one folder push. Watch uses the watched-inbox stability
// defaults and rules; a one-shot run processes current files immediately.
// Progress receives each acknowledged outcome and Failure each file the run
// skipped after the daemon or the local source rejected it.
type Options struct {
	Folder     config.WatchConfig
	Duplicates string
	Watch      bool
	Progress   func(ref, outcome string) error
	Failure    func(ref string, err error) error
}

// Report counts only independently acknowledged file outcomes, plus the files
// the run skipped after a rejection. Failed runs return their partial report
// and an error; callers may safely run again.
type Report struct {
	Added            int
	Updated          int
	Linked           int
	Skipped          int
	DuplicateSkipped int
	Failed           int
}

// Run executes a folder push without discovering or starting a local daemon.
// A file the daemon rejects is reported and skipped. Transport failures and
// daemon-side errors stop a one-shot run; watch mode observes the file again
// after a fresh settle window. A failed key proof, an invalid daemon response,
// a refused key, or a push name in use by a daemon watch stops either mode.
func Run(ctx context.Context, connection *daemonconn.Connection, opts Options) (Report, error) {
	var report Report
	if connection == nil {
		return report, errors.New("push requires a daemon connection")
	}
	if err := store.ValidatePushDuplicates(opts.Duplicates); err != nil {
		return report, err
	}
	p := &pusher{connection: connection, opts: opts, report: &report}
	scanner, err := ingest.NewFolderScanner(opts.Folder, p.process)
	if err != nil {
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

type pusher struct {
	connection *daemonconn.Connection
	opts       Options
	report     *Report
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
	case "linked":
		p.report.Linked++
	case "skipped":
		p.report.Skipped++
	case "duplicate_skipped":
		p.report.DuplicateSkipped++
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
	if daemonconn.IsKeyProofError(err) || daemonconn.IsResponseDecodeError(err) || errors.Is(err, errInvalidCursor) {
		return true
	}
	if code, ok := daemonconn.ProblemCode(err); ok && code == "push_name_in_use" {
		return true
	}
	status, ok := daemonconn.ResponseStatus(err)
	return ok && (status == http.StatusUnauthorized || status == http.StatusForbidden)
}

func retryable(err error) bool {
	if daemonconn.IsTransportError(err) {
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
	digest, err := hashSource(ctx, file)
	if err != nil {
		return "", err
	}
	state, err := p.connection.API().GetPushSource(ctx, &apiclient.GetPushSourceRequestOptions{
		Query: &apiclient.GetPushSourceQuery{PushName: p.opts.Folder.Name, SourceRef: file.Ref},
	})
	if err != nil {
		return "", err
	}
	if state.Known && (state.Node == nil || state.Node.ID <= 0 || state.Node.Kind != "file" ||
		state.Node.Revision <= 0 || state.Node.TrashedAt != "" || state.Hash == "" || state.Size < 0) {
		return "", errInvalidCursor
	}
	if state.Known && state.Hash == digest && state.Size == file.Info.Size() {
		return "skipped", nil
	}
	mimeType, err := detectMIME(file)
	if err != nil {
		return "", err
	}
	receipt, err := p.connection.PushUpload(ctx, store.PushSource{
		Name: p.opts.Folder.Name, Ref: file.Ref, Duplicates: p.opts.Duplicates,
		ModifiedAt: file.Info.ModTime().UTC().Format(time.RFC3339Nano),
	}, store.PushContent{
		ParentPath: path.Join(p.opts.Folder.Destination, path.Dir(file.Ref)),
		Name:       path.Base(file.Ref), Hash: digest, Size: file.Info.Size(), MIMEType: mimeType,
	}, contextReader{ctx: ctx, reader: file.File})
	if err != nil {
		return "", err
	}
	return receipt.Status, nil
}

// hashSource digests the opened file and confirms it did not change while
// being read.
func hashSource(ctx context.Context, file ingest.FolderFile) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, contextReader{ctx: ctx, reader: file.File})
	if err != nil {
		return "", fmt.Errorf("hashing source: %w", err)
	}
	if n != file.Info.Size() {
		return "", ingest.ErrSourceChanged
	}
	after, err := file.File.Stat()
	if err != nil {
		return "", fmt.Errorf("checking hashed source: %w", err)
	}
	if after.Size() != file.Info.Size() || !after.ModTime().Equal(file.Info.ModTime()) {
		return "", ingest.ErrSourceChanged
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// detectMIME sniffs the file head and leaves the file positioned at its start.
func detectMIME(file ingest.FolderFile) (string, error) {
	if _, err := file.File.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewinding source: %w", err)
	}
	head := make([]byte, 512)
	n, err := file.File.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading file type: %w", err)
	}
	if _, err := file.File.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewinding source: %w", err)
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
	if err == nil {
		err = r.ctx.Err()
	}
	return n, err
}
