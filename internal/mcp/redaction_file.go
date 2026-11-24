package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/filepublish"
)

type redactionFileOutput struct {
	DestinationPath string `json:"destination_path"`
	Size            int    `json:"size"`
	SHA256          string `json:"sha256"`
	State           string `json:"state"`
	CleanupFailed   bool   `json:"cleanup_failed"`
}

func saveRedactionFile(ctx context.Context, logger *slog.Logger, prefix, destination string, data []byte) (output redactionFileOutput, _ error) {
	if err := ctx.Err(); err != nil {
		return output, err
	}
	stage, err := filepublish.CreateStage(filepath.Dir(destination), prefix)
	if err != nil {
		return output, err
	}
	defer func() {
		if err := stage.Cleanup(); err != nil {
			logger.Warn("MCP redaction file stage cleanup failed", "error", err)
			output.CleanupFailed = true
		}
	}()
	if _, err := stage.File.Write(data); err != nil {
		return output, err
	}
	if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return output, err
	}
	if err := ctx.Err(); err != nil {
		return output, err
	}
	published, err := filepublish.Publish(stage.Path(), destination, false)
	if !published {
		return output, err
	}
	state := "published"
	if err != nil {
		state = "published_durability_unknown"
		logger.Warn("MCP redaction file saved with uncertain durability", "error", err)
	}
	return redactionFileOutput{DestinationPath: destination, State: state, Size: len(data), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}, nil
}

func redactionFileHandler[Output any](operation string, run func(context.Context, *daemonLease, []byte, *slog.Logger) (Output, error),
	lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := run(ctx, lease, request.Params.Arguments, logger)
		if err != nil {
			logOperationError(logger, operation, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok && invalid.Code == jsonrpc.CodeInvalidParams {
				return nil, invalid
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
