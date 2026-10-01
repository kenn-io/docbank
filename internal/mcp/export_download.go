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

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
)

var (
	errExportIntegrity = errors.New("export verification failed")
	errExportLocalIO   = errors.New("export file operation failed")
	publishExportFile  = filepublish.Publish
	cleanupExportStage = (*filepublish.Stage).Cleanup
)

type exportDownloadInput struct {
	JobID           string `json:"job_id"`
	DestinationPath string `json:"destination_path"`
	Overwrite       bool   `json:"overwrite"`
}

type exportDownloadOutput struct {
	privateCache

	JobID           string         `json:"job_id"`
	DestinationPath string         `json:"destination_path"`
	Receipt         bundle.Receipt `json:"receipt"`
	State           string         `json:"state"`
	CleanupFailed   bool           `json:"cleanup_failed"`
}

func downloadExport(
	ctx context.Context, lease *daemonLease, raw []byte, logger *slog.Logger,
) (output exportDownloadOutput, err error) {
	var input exportDownloadInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return output, err
	}
	if err := validateExportDestination(input.DestinationPath, input.Overwrite); err != nil {
		if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok {
			return output, invalid
		}
		return output, fmt.Errorf("%w: check destination: %w", errExportLocalIO, err)
	}
	parent := filepath.Dir(input.DestinationPath)
	stage, err := filepublish.CreateStage(parent, ".docbank-native-export-")
	if err != nil {
		return output, fmt.Errorf("%w: create stage: %w", errExportLocalIO, err)
	}
	defer func() {
		cleanupErr := cleanupExportStage(stage)
		if cleanupErr == nil {
			return
		}
		logger.Warn("MCP export stage cleanup failed",
			"error_code", "export_local_io", "error", cleanupErr)
		if output.State != "" {
			output.CleanupFailed = true
		}
	}()
	current, err := lease.acquire(ctx)
	if err != nil {
		return output, err
	}
	receipt, err := current.daemonconn.DownloadExportArchiveTo(ctx, input.JobID, stage.File)
	if err != nil {
		// Ticket streaming returns ordinary network/read errors, unlike generated API calls.
		var networkError net.Error
		if daemonconn.IsTransportError(err) || daemonconn.IsResponseDecodeError(err) ||
			errors.As(err, &networkError) || errors.Is(err, io.ErrUnexpectedEOF) ||
			errors.Is(err, io.EOF) || ctx.Err() != nil {
			lease.discard(current)
		}
		return output, exportDownloadError(ctx, err)
	}
	if err := stage.File.Sync(); err != nil {
		return output, fmt.Errorf("%w: sync stage: %w", errExportLocalIO, err)
	}
	if err := stage.File.Close(); err != nil {
		return output, fmt.Errorf("%w: close stage: %w", errExportLocalIO, err)
	}
	if err := ctx.Err(); err != nil {
		return output, err
	}
	published, err := publishExportFile(stage.Path(), input.DestinationPath, input.Overwrite)
	if !published {
		return output, fmt.Errorf("%w: publish: %w", errExportLocalIO, err)
	}
	state := "published"
	if err != nil {
		state = "published_durability_unknown"
		logger.Warn("MCP export published with uncertain durability", "error", err)
	}
	return exportDownloadOutput{
		privateCache: newPrivateCache(), JobID: input.JobID, DestinationPath: input.DestinationPath,
		Receipt: receipt, State: state,
	}, nil
}

func exportDownloadError(ctx context.Context, err error) error {
	var fileErr *os.PathError
	switch {
	case errors.Is(err, daemonconn.ErrIntegrity):
		return errExportIntegrity
	case errors.Is(err, bundle.ErrConflict):
		return bundle.ErrConflict
	case errors.As(err, &fileErr):
		return fmt.Errorf("%w: archive file: %w", errExportLocalIO, err)
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		return canceled
	}
	return sanitizedDaemonError(errDaemonRequestFailed, err)
}
