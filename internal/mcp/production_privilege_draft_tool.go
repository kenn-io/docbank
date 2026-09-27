package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionPrivilegeDraftOutput struct {
	privateCache

	LogID      string `json:"log_id"`
	Revision   int64  `json:"revision"`
	Generation int64  `json:"generation"`
}

func createProductionPrivilegeDraft(ctx context.Context, lease *daemonLease, raw []byte) (productionPrivilegeDraftOutput, error) {
	if len(raw) > 8192 {
		return productionPrivilegeDraftOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		LogID                    string `json:"log_id"`
		Revision                 int64  `json:"revision"`
		OperationID              string `json:"operation_id"`
		SetID                    string `json:"set_id"`
		SetRevision              int64  `json:"set_revision"`
		PlayersSHA256            string `json:"players_sha256"`
		RowsFile                 string `json:"rows_file"`
		PredecessorLogID         string `json:"predecessor_log_id"`
		PredecessorReceiptSHA256 string `json:"predecessor_receipt_sha256"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPrivilegeDraftOutput{}, err
	}
	if !validToolUUID(input.LogID) || input.Revision < 1 || !validToolUUID(input.OperationID) ||
		!validToolUUID(input.SetID) || input.SetRevision < 1 || !canonical.IsSHA256Hex(input.PlayersSHA256) ||
		((input.PredecessorLogID == "") != (input.PredecessorReceiptSHA256 == "")) ||
		(input.PredecessorLogID != "" && (!validToolUUID(input.PredecessorLogID) ||
			!canonical.IsSHA256Hex(input.PredecessorReceiptSHA256))) {
		return productionPrivilegeDraftOutput{}, invalidToolArgumentsError()
	}
	rows, err := readPrivatePrivilegeRowsFile(input.RowsFile)
	if err != nil {
		return productionPrivilegeDraftOutput{}, err
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.ProductionPrivilegeDraftGeneration, error) {
		return c.CreateProductionPrivilegeLogDraft(ctx, input.LogID, input.Revision,
			api.ProductionPrivilegeDraftCreateRequest{
				OperationID: input.OperationID, SetID: input.SetID, SetRevision: input.SetRevision,
				PlayersSHA256: input.PlayersSHA256, Rows: rows,
				PredecessorLogID:         input.PredecessorLogID,
				PredecessorReceiptSHA256: input.PredecessorReceiptSHA256,
			})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPrivilegeDraftOutput{}, errProductionPrivilegeOutcomeUnknown
		}
		return productionPrivilegeDraftOutput{}, err
	}
	return productionPrivilegeDraftOutput{privateCache: newPrivateCache(),
		LogID: result.LogID, Revision: result.Revision, Generation: result.Generation}, nil
}

func createProductionPrivilegeDraftToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionPrivilegeDraft(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionPrivilegeDraftToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
