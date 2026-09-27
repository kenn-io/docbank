package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	productionservice "go.kenn.io/docbank/internal/production"
)

var errProductionPrivilegeExportDestinationExists = errors.New("privilege export destination already exists")

type productionPrivilegeExportInput struct {
	LogID           string `json:"log_id"`
	Revision        int64  `json:"revision"`
	Format          string `json:"format"`
	DestinationPath string `json:"destination_path"`
	Overwrite       bool   `json:"overwrite"`
}

type productionPrivilegeExportOutput struct {
	privateCache

	LogID           string `json:"log_id"`
	Revision        int64  `json:"revision"`
	Format          string `json:"format"`
	DestinationPath string `json:"destination_path"`
	MediaType       string `json:"media_type"`
	ReceiptSHA256   string `json:"receipt_sha256"`
	RowsSHA256      string `json:"rows_sha256"`
	ContentSHA256   string `json:"content_sha256"`
	Size            int64  `json:"size"`
	State           string `json:"state"`
}

func exportProductionPrivilegeLog(ctx context.Context, lease *daemonLease,
	raw []byte) (productionPrivilegeExportOutput, error) {
	var empty productionPrivilegeExportOutput
	if len(raw) > 8192 {
		return empty, invalidToolArgumentsError()
	}
	var input productionPrivilegeExportInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return empty, err
	}
	if !validToolUUID(input.LogID) || input.Revision < 1 ||
		(input.Format != "json" && input.Format != "csv" && input.Format != "xlsx" && input.Format != "pdf") ||
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
			return empty, errProductionPrivilegeExportDestinationExists
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, err
	}
	exported, err := daemonRead(ctx, lease, func(ctx context.Context,
		c *daemonconn.Connection) (productionservice.PrivilegeLogExport, error) {
		return c.ExportProductionPrivilegeLog(ctx, input.LogID, input.Revision, input.Format)
	})
	if err != nil {
		return empty, err
	}
	stage, err := filepublish.CreateStage(parent, ".docbank-privilege-")
	if err != nil {
		return empty, err
	}
	defer func() { _ = stage.Cleanup() }()
	if _, err := stage.File.Write(exported.Content); err != nil {
		return empty, err
	}
	if err := stage.File.Sync(); err != nil {
		return empty, err
	}
	if err := stage.File.Close(); err != nil {
		return empty, err
	}
	published, publishErr := filepublish.Publish(stage.Path(), input.DestinationPath, input.Overwrite)
	if !published {
		if errors.Is(publishErr, os.ErrExist) {
			return empty, errProductionPrivilegeExportDestinationExists
		}
		return empty, publishErr
	}
	state := "published"
	if publishErr != nil {
		state = "published_durability_unknown"
	}
	return productionPrivilegeExportOutput{privateCache: newPrivateCache(),
		LogID: input.LogID, Revision: input.Revision, Format: input.Format,
		DestinationPath: input.DestinationPath, MediaType: exported.MediaType,
		ReceiptSHA256: exported.ReceiptSHA256, RowsSHA256: exported.RowsSHA256,
		ContentSHA256: exported.ContentSHA256, Size: int64(len(exported.Content)), State: state}, nil
}

func exportProductionPrivilegeLogToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := exportProductionPrivilegeLog(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, exportProductionPrivilegeLogToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
