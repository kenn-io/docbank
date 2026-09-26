package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

const (
	maxReportSpoolBytes  int64 = 512 << 20
	maxOpenReportHandles       = 16
)

var errReportSpoolCapacity = errors.New("report artifact spool capacity is exhausted")

// Bundle verification keeps the compressed ZIP and its decoded members in
// memory. A single process-wide slot prevents multiple MCP servers from each
// spending a full report verification budget at the same time.
var reportBundleVerificationSlot = make(chan struct{}, 1)

func acquireReportBundleVerification(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case reportBundleVerificationSlot <- struct{}{}:
		return func() { <-reportBundleVerificationSlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// reportSpool holds only a fully verified artifact. The file is created with
// private permissions and unlinked while open on platforms that allow it.
type reportSpool struct {
	metadata reportHandle
	file     *os.File
	path     string
	dir      string
	pin      *os.File
	size     int64
	timer    *time.Timer
}

func createVerifiedReportSpool(ctx context.Context, stream *daemonconn.TermReportStream,
	format string,
) (_ reportSpool, retErr error) {
	spool, err := createPrivateReportSpoolAt(os.TempDir())
	if err != nil {
		return reportSpool{}, err
	}
	defer func() {
		if retErr != nil {
			cleanupReportSpool(&spool)
		}
	}()
	if _, err := stream.CopyVerified(spool.file); err != nil {
		return reportSpool{}, err
	}
	if format == "bundle" {
		release, err := acquireReportBundleVerification(ctx)
		if err != nil {
			return reportSpool{}, err
		}
		defer release()
		if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
			return reportSpool{}, err
		}
		budget := report.NewBudget(report.DefaultBudgetBytes)
		defer func() { _ = budget.Close() }()
		if _, err := report.VerifyBundle(ctx, budget, spool.file, stream.Size); err != nil {
			return reportSpool{}, err
		}
	}
	if info, err := spool.file.Stat(); err != nil || info.Size() != stream.Size {
		return reportSpool{}, errReportHandleUnavailable
	}
	// Unix removes the name immediately. Windows retains it until the handle
	// closes; timer and explicit close both remove that remaining name.
	if err := os.Remove(spool.path); err == nil {
		spool.path = ""
	}
	return spool, nil
}

// verifyCSVSpool binds a downloaded CSV to the same retained report's
// independently checked evidence packet before the CSV spool can be published.
func verifyCSVSpool(ctx context.Context, client *daemonconn.Connection, id string,
	stream *daemonconn.TermReportStream, spool *reportSpool,
) error {
	release, err := acquireReportBundleVerification(ctx)
	if err != nil {
		return err
	}
	defer release()
	summary, err := client.GetTermReport(ctx, id)
	if err != nil {
		return err
	}
	if summary.State != "complete" || summary.CSVBytes != stream.Size || summary.CSVSHA256 != stream.SHA256 {
		return report.ErrInvalidPacket
	}
	companionStream, err := client.OpenTermReport(ctx, id, "bundle")
	if err != nil {
		return err
	}
	defer func() { _ = companionStream.Close() }()
	if companionStream.Size != summary.BundleBytes || companionStream.SHA256 != summary.BundleSHA256 {
		return report.ErrInvalidPacket
	}
	companion, err := createPrivateReportSpoolAt(os.TempDir())
	if err != nil {
		return err
	}
	defer cleanupReportSpool(&companion)
	if _, err := companionStream.CopyVerified(companion.file); err != nil {
		return err
	}
	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := companion.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	budget := report.NewBudget(report.DefaultBudgetBytes)
	defer func() { _ = budget.Close() }()
	if err := report.VerifyCSVArtifact(ctx, budget, summary, spool.file, companion.file); err != nil {
		return err
	}
	current, err := client.GetTermReport(ctx, id)
	if err != nil {
		return err
	}
	if current.State != "complete" || current.CSVBytes != summary.CSVBytes ||
		current.CSVSHA256 != summary.CSVSHA256 || current.BundleBytes != summary.BundleBytes ||
		current.BundleSHA256 != summary.BundleSHA256 {
		return report.ErrInvalidPacket
	}
	return nil
}

func (signer *reportHandleSigner) reserve(size int64) error {
	if size < 1 || size > maxReportSpoolBytes {
		return errReportSpoolCapacity
	}
	signer.mu.Lock()
	defer signer.mu.Unlock()
	if signer.closed {
		return errReportHandleUnavailable
	}
	signer.sweepExpiredLocked()
	if len(signer.spools)+signer.opening >= maxOpenReportHandles ||
		signer.reservedBytes > maxReportSpoolBytes-size {
		return errReportSpoolCapacity
	}
	signer.reservedBytes += size
	signer.opening++
	return nil
}

func (signer *reportHandleSigner) abort(size int64) {
	signer.mu.Lock()
	signer.reservedBytes -= size
	signer.opening--
	signer.mu.Unlock()
}

func (signer *reportHandleSigner) discardUnpublished(spool *reportSpool) {
	signer.abort(spool.size)
	cleanupReportSpool(spool)
}

func (signer *reportHandleSigner) publish(token string, spool *reportSpool) error {
	signer.mu.Lock()
	signer.opening--
	if _, exists := signer.spools[token]; signer.closed || exists || time.Now().Unix() >= spool.metadata.Expires {
		signer.reservedBytes -= spool.size
		signer.mu.Unlock()
		cleanupReportSpool(spool)
		return errReportHandleUnavailable
	}
	signer.spools[token] = spool
	spool.timer = time.AfterFunc(time.Until(time.Unix(spool.metadata.Expires, 0)), func() {
		signer.drop(token)
	})
	signer.mu.Unlock()
	return nil
}

func (signer *reportHandleSigner) read(token string, handle reportHandle,
	offset, limit int64, closeAfter bool,
) ([]byte, error) {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	signer.sweepExpiredLocked()
	spool := signer.spools[token]
	if spool == nil || spool.metadata != handle || spool.file == nil {
		return nil, errReportHandleUnavailable
	}
	info, err := spool.file.Stat()
	if err != nil || info.Size() != handle.Size {
		signer.dropLocked(token)
		return nil, errReportHandleUnavailable
	}
	chunk := make([]byte, int(min(limit, handle.Size-offset)))
	if len(chunk) != 0 {
		if _, err := spool.file.ReadAt(chunk, offset); err != nil {
			signer.dropLocked(token)
			return nil, errReportHandleUnavailable
		}
	}
	if closeAfter {
		signer.dropLocked(token)
	}
	return chunk, nil
}

func (signer *reportHandleSigner) drop(token string) {
	signer.mu.Lock()
	signer.dropLocked(token)
	signer.mu.Unlock()
}

func (signer *reportHandleSigner) dropLocked(token string) {
	spool := signer.spools[token]
	if spool == nil {
		return
	}
	delete(signer.spools, token)
	signer.reservedBytes -= spool.size
	if spool.timer != nil {
		spool.timer.Stop()
	}
	cleanupReportSpool(spool)
}

func (signer *reportHandleSigner) sweepExpiredLocked() {
	for token, spool := range signer.spools {
		if time.Now().Unix() >= spool.metadata.Expires {
			signer.dropLocked(token)
		}
	}
}

func (signer *reportHandleSigner) closeAll() {
	if signer == nil {
		return
	}
	signer.mu.Lock()
	signer.closed = true
	for token := range signer.spools {
		signer.dropLocked(token)
	}
	signer.mu.Unlock()
}

func cleanupReportSpool(spool *reportSpool) {
	if spool == nil {
		return
	}
	if spool.file != nil {
		_ = spool.file.Close()
		spool.file = nil
	}
	if spool.path != "" {
		_ = os.Remove(spool.path)
		spool.path = ""
	}
	if spool.pin != nil {
		_ = spool.pin.Close()
		spool.pin = nil
	}
	if spool.dir != "" {
		_ = os.Remove(spool.dir)
		spool.dir = ""
	}
}
