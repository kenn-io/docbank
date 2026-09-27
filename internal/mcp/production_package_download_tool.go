package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
)

var (
	errProductionPackageDestinationExists = errors.New("production package destination already exists")
	errProductionPackageDownloadFailed    = errors.New("production package download failed")
)

type productionPackageDownloadOutput struct {
	privateCache

	JobID           string `json:"job_id"`
	OperationID     string `json:"operation_id"`
	DestinationPath string `json:"destination_path"`
	ArchiveSHA256   string `json:"archive_sha256"`
	VersionID       string `json:"version_id"`
	Size            int64  `json:"size"`
	State           string `json:"state"`
}

type productionPackageDownloadedStage struct {
	stage  *filepublish.Stage
	ticket api.ProductionPackageDownloadTicket
}

func downloadProductionPackage(ctx context.Context, lease *daemonLease, raw []byte) (productionPackageDownloadOutput, error) {
	var empty productionPackageDownloadOutput
	if len(raw) > 8192 {
		return empty, invalidToolArgumentsError()
	}
	var input struct {
		JobID           string `json:"job_id"`
		OperationID     string `json:"operation_id"`
		DestinationPath string `json:"destination_path"`
		Overwrite       bool   `json:"overwrite"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return empty, err
	}
	if !validToolUUID(input.JobID) || !validToolUUID(input.OperationID) ||
		len(input.DestinationPath) > maxPathCharacters || !filepath.IsAbs(input.DestinationPath) ||
		filepath.Clean(input.DestinationPath) != input.DestinationPath {
		return empty, invalidToolArgumentsError()
	}
	parent := filepath.Dir(input.DestinationPath)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return empty, invalidToolArgumentsError()
	}
	if destination, err := os.Lstat(input.DestinationPath); err == nil {
		if destination.Mode()&os.ModeSymlink != 0 || !destination.Mode().IsRegular() {
			return empty, invalidToolArgumentsError()
		}
		if !input.Overwrite {
			return empty, errProductionPackageDestinationExists
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, errProductionPackageDownloadFailed
	}
	downloaded, err := daemonRead(ctx, lease, func(ctx context.Context,
		c *daemonconn.Connection) (productionPackageDownloadedStage, error) {
		stage, err := filepublish.CreateStage(parent, ".docbank-production-package-")
		if err != nil {
			return productionPackageDownloadedStage{}, err
		}
		ticket, err := c.DownloadProductionPackageTo(ctx, input.JobID, input.OperationID, stage.File)
		if err != nil {
			_ = stage.Cleanup()
			return productionPackageDownloadedStage{}, err
		}
		if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
			_ = stage.Cleanup()
			return productionPackageDownloadedStage{}, err
		}
		return productionPackageDownloadedStage{stage: stage, ticket: ticket}, nil
	})
	if err != nil {
		if errors.Is(err, errDaemonUnavailable) {
			return empty, err
		}
		return empty, errProductionPackageDownloadFailed
	}
	defer func() { _ = downloaded.stage.Cleanup() }()
	published, publishErr := filepublish.Publish(downloaded.stage.Path(), input.DestinationPath, input.Overwrite)
	if !published {
		if errors.Is(publishErr, os.ErrExist) {
			return empty, errProductionPackageDestinationExists
		}
		return empty, errProductionPackageDownloadFailed
	}
	state := "published"
	if publishErr != nil {
		state = "published_durability_unknown"
	}
	return productionPackageDownloadOutput{privateCache: newPrivateCache(),
		JobID: input.JobID, OperationID: input.OperationID, DestinationPath: input.DestinationPath,
		ArchiveSHA256: downloaded.ticket.ArchiveSHA256, VersionID: downloaded.ticket.VersionID,
		Size: downloaded.ticket.Size, State: state}, nil
}

func downloadProductionPackageToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := downloadProductionPackage(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, downloadProductionPackageToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
