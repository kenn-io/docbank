package daemonconn

import (
	"context"
	"errors"
	"io"
	"os"

	"go.kenn.io/docbank/report"
)

// ErrReportReviewRequired means the frozen report has no downloadable counts yet.
var ErrReportReviewRequired = errors.New("report needs date review")

// TermReportDownload describes a packet checked against its frozen summary.
type TermReportDownload struct {
	Size         int64
	SHA256       string
	Verification report.Verification
}

// DownloadTermReportTo verifies a report in an empty caller-owned staging file.
// The caller owns its budget, publication, and cleanup. This method never publishes.
func (c *Connection) DownloadTermReportTo(
	ctx context.Context, id string, output *os.File, budget report.Budget,
) (TermReportDownload, error) {
	summary, err := c.GetTermReport(ctx, id)
	if err != nil {
		return TermReportDownload{}, err
	}
	if summary.State == "needs_review" {
		return TermReportDownload{}, ErrReportReviewRequired
	}
	stream, err := c.OpenTermReport(ctx, id, "bundle")
	if err != nil {
		return TermReportDownload{}, err
	}
	defer func() { _ = stream.Close() }()
	if stream.Size != summary.BundleBytes || stream.SHA256 != summary.BundleSHA256 {
		return TermReportDownload{}, integrityErrorf("report stream differs from frozen summary")
	}
	if _, err := stream.CopyVerified(output); err != nil {
		return TermReportDownload{}, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return TermReportDownload{}, err
	}
	verified, err := report.VerifyBundle(ctx, budget, output, stream.Size)
	if err != nil {
		var fileErr *os.PathError
		if errors.As(err, &fileErr) || errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) || errors.Is(err, report.ErrBudgetExhausted) ||
			errors.Is(err, report.ErrBudgetClosed) || errors.Is(err, report.ErrReportLimit) {
			return TermReportDownload{}, err
		}
		return TermReportDownload{}, integrityErrorf("report packet verification failed: %v", err)
	}
	return TermReportDownload{Size: stream.Size, SHA256: stream.SHA256, Verification: verified}, nil
}
