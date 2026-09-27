package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

const maxProductionPrivilegeRowsInputBytes = 64 << 20

type productionPrivilegeRowsOutput struct {
	privateCache

	LogID      string `json:"log_id"`
	Revision   int64  `json:"revision"`
	Generation int64  `json:"generation"`
}

func readPrivatePrivilegeRowsFile(path string) ([]documentproduction.PrivilegeRow, error) {
	if len(path) == 0 || len(path) > maxPathCharacters || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path {
		return nil, invalidToolArgumentsError()
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProductionPrivilegeRowsInputBytes {
		return nil, invalidToolArgumentsError()
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, invalidToolArgumentsError()
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, invalidToolArgumentsError()
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxProductionPrivilegeRowsInputBytes+1))
	if err != nil || len(raw) > maxProductionPrivilegeRowsInputBytes {
		return nil, invalidToolArgumentsError()
	}
	var rows []documentproduction.PrivilegeRow
	if err := json.Unmarshal(raw, &rows, json.RejectUnknownMembers(true)); err != nil ||
		len(rows) == 0 || len(rows) > documentproduction.MaxPrivilegeRows {
		return nil, invalidToolArgumentsError()
	}
	if _, _, err := documentproduction.CanonicalPrivilegeRows(rows); err != nil {
		return nil, invalidToolArgumentsError()
	}
	return rows, nil
}

func replaceProductionPrivilegeRows(ctx context.Context, lease *daemonLease, raw []byte) (productionPrivilegeRowsOutput, error) {
	if len(raw) > 8192 {
		return productionPrivilegeRowsOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		LogID              string `json:"log_id"`
		Revision           int64  `json:"revision"`
		OperationID        string `json:"operation_id"`
		ExpectedGeneration int64  `json:"expected_generation"`
		RowsFile           string `json:"rows_file"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPrivilegeRowsOutput{}, err
	}
	if !validToolUUID(input.LogID) || input.Revision < 1 || !validToolUUID(input.OperationID) ||
		input.ExpectedGeneration < 1 {
		return productionPrivilegeRowsOutput{}, invalidToolArgumentsError()
	}
	rows, err := readPrivatePrivilegeRowsFile(input.RowsFile)
	if err != nil {
		return productionPrivilegeRowsOutput{}, err
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.ProductionPrivilegeDraftGeneration, error) {
		return c.ReplaceProductionPrivilegeLogRows(ctx, input.LogID, input.Revision,
			api.ProductionPrivilegeRowsReplaceRequest{
				OperationID: input.OperationID, ExpectedGeneration: input.ExpectedGeneration, Rows: rows,
			})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPrivilegeRowsOutput{}, errProductionPrivilegeOutcomeUnknown
		}
		return productionPrivilegeRowsOutput{}, err
	}
	return productionPrivilegeRowsOutput{privateCache: newPrivateCache(),
		LogID: result.LogID, Revision: result.Revision, Generation: result.Generation}, nil
}

func replaceProductionPrivilegeRowsToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := replaceProductionPrivilegeRows(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, replaceProductionPrivilegeRowsToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
