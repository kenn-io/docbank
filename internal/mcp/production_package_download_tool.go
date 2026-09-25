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

var errProductionDownloadFailed = errors.New("production package download failed verification")

type productionPackageDownloadOutput struct {
	privateCache

	JobID           string `json:"job_id"`
	OperationID     string `json:"operation_id"`
	DestinationPath string `json:"destination_path"`
	VersionID       string `json:"version_id"`
	ArchiveSHA256   string `json:"archive_sha256"`
	Size            int64  `json:"size"`
	State           string `json:"state"`
}

func downloadProductionPackage(ctx context.Context, lease *daemonLease, raw []byte) (productionPackageDownloadOutput, error) {
	var input struct {
		JobID           string `json:"job_id"`
		OperationID     string `json:"operation_id"`
		DestinationPath string `json:"destination_path"`
		Overwrite       bool   `json:"overwrite"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPackageDownloadOutput{}, err
	}
	if !validToolUUID(input.JobID) || !validToolUUID(input.OperationID) ||
		!filepath.IsAbs(input.DestinationPath) || filepath.Clean(input.DestinationPath) != input.DestinationPath {
		return productionPackageDownloadOutput{}, invalidToolArgumentsError()
	}
	parent := filepath.Dir(input.DestinationPath)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return productionPackageDownloadOutput{}, invalidToolArgumentsError()
	}
	if destination, err := os.Lstat(input.DestinationPath); err == nil {
		if destination.Mode()&os.ModeSymlink != 0 || !destination.Mode().IsRegular() {
			return productionPackageDownloadOutput{}, invalidToolArgumentsError()
		}
		if !input.Overwrite {
			return productionPackageDownloadOutput{}, os.ErrExist
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return productionPackageDownloadOutput{}, err
	}
	stage, err := filepublish.CreateStage(parent, ".docbank-production-")
	if err != nil {
		return productionPackageDownloadOutput{}, err
	}
	staged, stagedPath := stage.File, stage.Path()
	keep := false
	defer func() {
		if !keep {
			_ = stage.Cleanup()
		}
	}()
	ticket, err := daemonRead(ctx, lease, func(ctx context.Context, connection *daemonconn.Connection) (api.ProductionPackageDownloadTicket, error) {
		return connection.DownloadProductionPackageTo(ctx, input.JobID, input.OperationID, staged)
	})
	if err != nil {
		if code, _ := stableDomainError(err); code == "" {
			return productionPackageDownloadOutput{}, errProductionDownloadFailed
		}
		return productionPackageDownloadOutput{}, err
	}
	if err := staged.Sync(); err != nil {
		return productionPackageDownloadOutput{}, err
	}
	if err := staged.Close(); err != nil {
		return productionPackageDownloadOutput{}, err
	}
	published, publishErr := filepublish.Publish(stagedPath, input.DestinationPath, input.Overwrite)
	if !published {
		return productionPackageDownloadOutput{}, publishErr
	}
	_ = os.Remove(stagedPath)
	keep = true
	_ = stage.Cleanup()
	state := "published"
	if publishErr != nil {
		state = "published_durability_unknown"
	}
	return productionPackageDownloadOutput{privateCache: newPrivateCache(), JobID: input.JobID,
		OperationID: input.OperationID, DestinationPath: input.DestinationPath,
		VersionID: ticket.VersionID, ArchiveSHA256: ticket.ArchiveSHA256,
		Size: ticket.Size, State: state}, nil
}

func productionPackageDownloadToolHandler(
	lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
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
