package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/report"
)

var (
	errReportIntegrity = errors.New("report verification failed")
	errReportLocalIO   = errors.New("report file operation failed")
	publishReportFile  = filepublish.Publish
	cleanupReportStage = (*filepublish.Stage).Cleanup
)

var reportDownloadTool = toolDefinition{name: "download_report", title: "Download report",
	write: true, destructive: true, schemas: downloadReportSchemas,
	description: "Save a verified frozen evidence ZIP on the MCP host. Review the destination with " +
		"the operator. Check bundle_bytes first; large downloads may exceed the HTTP deadline. " +
		"Download does not free a report handle. " +
		"If the reply is lost, inspect the saved file before retrying."}

type reportDownloadInput struct {
	ReportID        string `json:"report_id"`
	DestinationPath string `json:"destination_path"`
	Overwrite       bool   `json:"overwrite"`
}

type reportVerificationOutput struct {
	InternallyConsistent bool `json:"internally_consistent"`
	SourceVerified       bool `json:"source_verified"`
}

type reportDownloadOutput struct {
	privateCache

	ReportID        string                   `json:"report_id"`
	DestinationPath string                   `json:"destination_path"`
	Bytes           int64                    `json:"bytes"`
	SHA256          string                   `json:"sha256"`
	Verification    reportVerificationOutput `json:"verification"`
	State           string                   `json:"state"`
	CleanupFailed   bool                     `json:"cleanup_failed"`
}

func (r *reportTools) download(
	ctx context.Context, raw []byte,
) (output reportDownloadOutput, err error) {
	var input reportDownloadInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return output, err
	}
	if err := validateExportDestination(input.DestinationPath, input.Overwrite); err != nil {
		if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok {
			return output, invalid
		}
		return output, fmt.Errorf("%w: check destination: %w", errReportLocalIO, err)
	}
	stage, err := filepublish.CreateStage(filepath.Dir(input.DestinationPath), ".docbank-report-")
	if err != nil {
		return output, fmt.Errorf("%w: create stage: %w", errReportLocalIO, err)
	}
	defer func() {
		if cleanupErr := cleanupReportStage(stage); cleanupErr != nil {
			r.logger.Warn("MCP report stage cleanup failed",
				"error_code", "report_local_io", "error", cleanupErr)
			if output.State != "" {
				output.CleanupFailed = true
			}
		}
	}()
	current, err := r.lease.acquire(ctx)
	if err != nil {
		return output, err
	}
	budget := r.budget.Child()
	defer func() { _ = budget.Close() }()
	verified, err := current.daemonconn.DownloadTermReportTo(ctx, input.ReportID, stage.File, budget)
	if err != nil {
		var networkError net.Error
		if daemonconn.IsTransportError(err) || daemonconn.IsResponseDecodeError(err) ||
			errors.As(err, &networkError) || errors.Is(err, io.ErrUnexpectedEOF) ||
			errors.Is(err, io.EOF) || ctx.Err() != nil {
			r.lease.discard(current)
		}
		return output, reportDownloadError(ctx, err)
	}
	if err := stage.File.Sync(); err != nil {
		return output, fmt.Errorf("%w: sync stage: %w", errReportLocalIO, err)
	}
	if err := stage.File.Close(); err != nil {
		return output, fmt.Errorf("%w: close stage: %w", errReportLocalIO, err)
	}
	if err := ctx.Err(); err != nil {
		return output, err
	}
	published, err := publishReportFile(stage.Path(), input.DestinationPath, input.Overwrite)
	if !published {
		return output, fmt.Errorf("%w: publish stage: %w", errReportLocalIO, err)
	}
	state := "published"
	if err != nil {
		state = "published_durability_unknown"
		r.logger.Warn("MCP report published with uncertain durability", "error", err)
	}
	return reportDownloadOutput{privateCache: newPrivateCache(), ReportID: input.ReportID,
		DestinationPath: input.DestinationPath, Bytes: verified.Size, SHA256: verified.SHA256,
		Verification: reportVerificationOutput{InternallyConsistent: verified.Verification.InternallyConsistent,
			SourceVerified: verified.Verification.SourceVerified}, State: state}, nil
}

func reportDownloadError(ctx context.Context, err error) error {
	var fileErr *os.PathError
	switch {
	case errors.Is(err, daemonconn.ErrIntegrity):
		return errReportIntegrity
	case errors.Is(err, daemonconn.ErrReportReviewRequired):
		return daemonconn.ErrReportReviewRequired
	case errors.Is(err, report.ErrReportLimit), errors.Is(err, report.ErrBudgetExhausted),
		errors.Is(err, report.ErrBudgetClosed):
		return report.ErrReportLimit
	case errors.As(err, &fileErr):
		return fmt.Errorf("%w: packet file: %w", errReportLocalIO, err)
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		return canceled
	}
	return sanitizedDaemonError(errDaemonRequestFailed, err)
}
