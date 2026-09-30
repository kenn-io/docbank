package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
)

// DownloadExportArchiveTo verifies a native bundle in a caller-owned staging
// file. Publication and explicit release remain the caller's responsibility.
func (c *Connection) DownloadExportArchiveTo(
	ctx context.Context, id string, destination *os.File,
) (bundle.Receipt, error) {
	if destination == nil {
		return bundle.Receipt{}, errors.New("export destination is required")
	}
	job, err := c.API().GetExportJob(ctx, &apiclient.GetExportJobRequestOptions{
		PathParams: &apiclient.GetExportJobPath{ID: id},
	})
	if err != nil {
		return bundle.Receipt{}, err
	}
	if job.ID != id {
		return bundle.Receipt{}, integrityErrorf("export job identity disagrees with request")
	}
	if job.State != "completed" {
		return bundle.Receipt{}, fmt.Errorf(
			"export is %s; download requires a completed job: %w", job.State, bundle.ErrConflict)
	}
	r := job.Receipt
	if r == nil || r.Format != bundle.Format || !canonical.IsSHA256Hex(r.PlanFingerprint) ||
		r.PlanFingerprint != job.Fingerprint || !canonical.IsSHA256Hex(r.SHA256) ||
		r.Size < 1 || r.Size > bundle.MaxArchiveBytes || r.Entries < 1 {
		return bundle.Receipt{}, integrityErrorf("export job receipt is inconsistent")
	}
	if err := destination.Truncate(0); err != nil {
		return bundle.Receipt{}, err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return bundle.Receipt{}, err
	}
	ticket, err := c.API().DownloadExportArchive(ctx,
		&apiclient.DownloadExportArchiveRequestOptions{
			PathParams: &apiclient.DownloadExportArchivePath{ID: id},
			Body:       &bundle.DownloadRequest{},
		})
	if err != nil {
		return bundle.Receipt{}, err
	}
	if ticket.Receipt != *r {
		return bundle.Receipt{}, integrityErrorf("export ticket disagrees with job receipt")
	}
	if err := c.copyExportArchive(ctx, ticket.URL, r.Size, destination); err != nil {
		return bundle.Receipt{}, err
	}
	verified, err := bundle.Verify(ctx, destination, r.Size, job.Fingerprint)
	if err != nil {
		var fileErr *os.PathError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.As(err, &fileErr) {
			return bundle.Receipt{}, err
		}
		return bundle.Receipt{}, integrityErrorf("export archive verification failed: %v", err)
	}
	if verified != *r {
		return bundle.Receipt{}, integrityErrorf("export archive disagrees with job receipt")
	}
	return verified, nil
}

func (c *Connection) copyExportArchive(
	ctx context.Context, ticket string, size int64, destination *os.File,
) error {
	response, err := c.openTicketDownload(ctx, ticket)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("export download failed with HTTP %d", response.StatusCode)
	}
	written, err := io.CopyBuffer(destination,
		io.LimitReader(response.Body, size+1), make([]byte, bundle.BufferSize))
	if err != nil {
		return fmt.Errorf("downloading export: %w", err)
	}
	if written != size {
		return integrityErrorf("export byte count disagrees with receipt")
	}
	return nil
}
