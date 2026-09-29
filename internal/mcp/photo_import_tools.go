package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type photoImportRunToolOutput struct {
	privateCache
	api.PhotoImportRun
}

type photoImportListToolOutput struct {
	privateCache

	Items []api.PhotoImportRun `json:"items"`
}

func photoImportRunOutput(run api.PhotoImportRun) photoImportRunToolOutput {
	return photoImportRunToolOutput{privateCache: newPrivateCache(), PhotoImportRun: run}
}

func listPhotoImports(ctx context.Context, lease *daemonLease) (photoImportListToolOutput, error) {
	runs, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) ([]api.PhotoImportRun, error) {
		return c.PhotoImports(ctx)
	})
	if err != nil {
		return photoImportListToolOutput{}, err
	}
	return photoImportListToolOutput{privateCache: newPrivateCache(), Items: runs}, nil
}

func getPhotoImport(ctx context.Context, lease *daemonLease, raw []byte) (photoImportRunToolOutput, error) {
	var input struct {
		RunID string `json:"run_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil || input.RunID == "" {
		return photoImportRunToolOutput{}, invalidToolArgumentsError()
	}
	run, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.PhotoImportRun, error) {
		return c.PhotoImport(ctx, input.RunID)
	})
	if err != nil {
		return photoImportRunToolOutput{}, err
	}
	return photoImportRunOutput(run), nil
}

func photoImportWriteToolHandler(lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executePhotoImportWriteTool(ctx, lease, name, validator, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return result, nil
	}
}

func executePhotoImportWriteTool(ctx context.Context, lease *daemonLease, name string, validator *jsonschema.Resolved, raw []byte) (*sdkmcp.CallToolResult, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return nil, err
	}
	var run api.PhotoImportRun
	var err error
	switch name {
	case "start_photo_import":
		var input api.PhotoImportStartRequest
		if err = decodeReadArguments(raw, &input); err == nil {
			run, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoImportRun, error) {
				return c.StartPhotoImport(ctx, input.SourceRoot, input.Destination, input.Choice)
			})
		}
	case "cancel_photo_import":
		var input struct {
			RunID    string `json:"run_id"`
			Revision int64  `json:"revision"`
		}
		if err = decodeReadArguments(raw, &input); err == nil {
			run, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PhotoImportRun, error) {
				return c.CancelPhotoImport(ctx, input.RunID, input.Revision)
			})
		}
	default:
		return nil, errors.New("unknown Docbank photo import write tool")
	}
	if err != nil {
		return nil, err
	}
	result, err := boundedToolSuccess(validator, photoImportRunOutput(run), nil)
	if err != nil {
		return nil, sanitizedDaemonError(errProcessingOutcomeUnknown,
			fmt.Errorf("photo import response failed output validation: %w", err))
	}
	return result, nil
}
