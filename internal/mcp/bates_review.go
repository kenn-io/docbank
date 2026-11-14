package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactionfiles"
)

// PDFium's backing and RSS limit belong to the process, including separate MCP sessions.
var reviewedBatesActive atomic.Bool

var cleanupBatesStage = (*filepublish.Stage).Cleanup

// reviewedBatesError scopes local verification outcomes to the reviewed export.
// Recovery-only exports keep their existing error contract.
type reviewedBatesError struct {
	code  string
	cause error
}

func (e *reviewedBatesError) Error() string {
	if e.code == "bates_local_io" {
		return fmt.Sprintf("%s: %v", e.code, e.cause)
	}
	return e.code
}
func (e *reviewedBatesError) Unwrap() error { return e.cause }

func (input exportBatesFileInput) reviewed() (bool, error) {
	paths := []string{input.PreparationPath, input.MasksPath, input.ReviewPath}
	if paths[0] == "" && paths[1] == "" && paths[2] == "" {
		return false, nil
	}
	for _, path := range paths {
		if path == "" || len(path) > maxPathCharacters || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return false, invalidToolArgumentsError()
		}
	}
	return true, nil
}

func exportReviewedBatesFile(ctx context.Context, lease *daemonLease, input exportBatesFileInput,
	logger *slog.Logger,
) (exportBatesFileOutput, error) {
	if !reviewedBatesActive.CompareAndSwap(false, true) {
		return exportBatesFileOutput{}, &reviewedBatesError{code: "redaction_busy"}
	}
	// Registered first: stage cleanup and verifier teardown finish before admission opens.
	defer reviewedBatesActive.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	request, err := redactionfiles.ReadInputs(ctx, input.PreparationPath, input.MasksPath)
	if err != nil {
		return exportBatesFileOutput{}, reviewedBatesInputError(ctx, err)
	}
	review, err := redactionfiles.ReadReview(ctx, input.ReviewPath, request)
	if err != nil {
		return exportBatesFileOutput{}, reviewedBatesInputError(ctx, err)
	}
	current, err := lease.acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return exportBatesFileOutput{}, reviewedBatesDownloadError(ctx, err)
		}
		return exportBatesFileOutput{}, daemonAcquisitionError(err)
	}
	c := current.daemonconn
	receipt, err := c.BatesExport(ctx, input.AllocationID)
	if err != nil {
		return exportBatesFileOutput{}, reviewedBatesDaemonError(ctx, lease, current, err)
	}
	numbered, err := daemonconn.CheckNumberedBatesExport(receipt)
	if err != nil {
		return exportBatesFileOutput{}, reviewedBatesDownloadError(ctx, err)
	}
	if numbered == nil {
		return exportBatesFileOutput{}, invalidToolArgumentsError()
	}
	stage, err := filepublish.CreateStage(filepath.Dir(input.DestinationPath), ".docbank-bates-")
	if err != nil {
		return exportBatesFileOutput{}, reviewedBatesLocalError(ctx, err)
	}
	defer func() {
		if err := cleanupBatesStage(stage); err != nil {
			logger.Warn("removing Bates export staging directory", "error_code", "bates_local_io", "error", err)
		}
	}()
	// This selects the reviewed contract, matches it before transfer and checks the
	// same staged handle. A refusal is never retried with a weaker verifier.
	verified, err := c.DownloadBatesExportTo(ctx, receipt, stage.File, &review)
	if err != nil {
		return exportBatesFileOutput{}, reviewedBatesDaemonError(ctx, lease, current, err)
	}
	if verified.Size != receipt.Size || verified.Verification != "masks_text_and_numbers" {
		return exportBatesFileOutput{}, &reviewedBatesError{code: "redaction_integrity"}
	}
	if err := stage.File.Sync(); err != nil {
		return exportBatesFileOutput{}, reviewedBatesLocalError(ctx, err)
	}
	if err := stage.File.Close(); err != nil {
		return exportBatesFileOutput{}, reviewedBatesLocalError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return exportBatesFileOutput{}, reviewedBatesDownloadError(ctx, err)
	}
	published, err := filepublish.Publish(stage.Path(), input.DestinationPath, input.Overwrite)
	if !published {
		return exportBatesFileOutput{}, reviewedBatesLocalError(ctx, err)
	}
	state := "published"
	if err != nil {
		state = "published_durability_unknown"
	}
	return exportBatesFileOutput{privateCache: newPrivateCache(), Verification: verified.Verification,
		AllocationID: receipt.AllocationID, ArtifactID: receipt.ArtifactID,
		DestinationPath: input.DestinationPath, BlobSHA256: receipt.BlobSHA256,
		ManifestSHA256: receipt.ManifestSHA256, Size: receipt.Size, State: state}, nil
}

func reviewedBatesInputError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, pdfproduction.ErrVerifierVersion) ||
		errors.Is(err, processing.ErrRedactionReviewChanged) {
		return reviewedBatesDownloadError(ctx, err)
	}
	// Reader and decoder errors can contain private text. Drop their causes before logging.
	return invalidToolArgumentsError()
}

func reviewedBatesLocalError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return reviewedBatesDownloadError(ctx, err)
	}
	if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok {
		return invalid
	}
	return &reviewedBatesError{code: "bates_local_io", cause: err}
}

func reviewedBatesDaemonError(ctx context.Context, lease *daemonLease, current leasedDaemonClient, err error) error {
	var networkError net.Error
	if daemonconn.IsTransportError(err) || daemonconn.IsResponseDecodeError(err) ||
		errors.As(err, &networkError) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) || ctx.Err() != nil {
		lease.discard(current)
	}
	return reviewedBatesDownloadError(ctx, err)
}

func reviewedBatesDownloadError(ctx context.Context, err error) error {
	var memory *pdfproduction.RSSLimitError
	var file *os.PathError
	var code string
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = "redaction_timeout"
	case ctx.Err() != nil:
		code = "redaction_canceled"
	case errors.As(err, &memory):
		code = "redaction_memory"
	case errors.Is(err, pdfproduction.ErrVerifierVersion):
		code = "redaction_verifier_version"
	case errors.Is(err, processing.ErrRedactionReviewChanged):
		code = "redaction_review_changed"
	case errors.Is(err, daemonconn.ErrIntegrity), errors.Is(err, pdfproduction.ErrPDFIntegrity):
		code = "redaction_integrity"
	case errors.Is(err, context.DeadlineExceeded):
		code = "redaction_worker_timeout"
	case errors.As(err, &file):
		return reviewedBatesLocalError(ctx, err)
	default:
		return sanitizedDaemonError(errDaemonRequestFailed, err)
	}
	return &reviewedBatesError{code: code, cause: err}
}
