package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionPrivilegeFreezeOutput struct {
	privateCache

	LogID         string `json:"log_id"`
	Revision      int64  `json:"revision"`
	RowCount      int    `json:"row_count"`
	ReceiptSHA256 string `json:"receipt_sha256"`
	InputsSHA256  string `json:"inputs_sha256"`
	FrozenAt      string `json:"frozen_at"`
}

func freezeProductionPrivilegeLog(ctx context.Context, lease *daemonLease, raw []byte) (productionPrivilegeFreezeOutput, error) {
	if len(raw) > 4096 {
		return productionPrivilegeFreezeOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		LogID                            string `json:"log_id"`
		Revision                         int64  `json:"revision"`
		OperationID                      string `json:"operation_id"`
		ExpectedGeneration               int64  `json:"expected_generation"`
		ExpectedInputsSHA256             string `json:"expected_inputs_sha256"`
		ExpectedApprovalEvaluationSHA256 string `json:"expected_approval_evaluation_sha256"`
		FrozenAt                         string `json:"frozen_at"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPrivilegeFreezeOutput{}, err
	}
	if !validToolUUID(input.LogID) || input.Revision < 1 || !validToolUUID(input.OperationID) ||
		input.ExpectedGeneration < 1 || !canonical.IsSHA256Hex(input.ExpectedInputsSHA256) ||
		input.ExpectedApprovalEvaluationSHA256 != "" &&
			!canonical.IsSHA256Hex(input.ExpectedApprovalEvaluationSHA256) || len(input.FrozenAt) > 64 {
		return productionPrivilegeFreezeOutput{}, invalidToolArgumentsError()
	}
	receipt, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (documentproduction.PrivilegeLogReceipt, error) {
		return c.FreezeProductionPrivilegeLog(ctx, input.LogID, input.Revision,
			api.ProductionPrivilegeFreezeRequest{
				OperationID: input.OperationID, ExpectedGeneration: input.ExpectedGeneration,
				ExpectedInputsSHA256:             input.ExpectedInputsSHA256,
				ExpectedApprovalEvaluationSHA256: input.ExpectedApprovalEvaluationSHA256,
				FrozenAt:                         input.FrozenAt,
			})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPrivilegeFreezeOutput{}, errProductionPrivilegeOutcomeUnknown
		}
		return productionPrivilegeFreezeOutput{}, err
	}
	return productionPrivilegeFreezeOutput{privateCache: newPrivateCache(),
		LogID: receipt.LogID, Revision: receipt.Revision, RowCount: receipt.RowCount,
		ReceiptSHA256: receipt.SHA256, InputsSHA256: receipt.InputsSHA256, FrozenAt: receipt.FrozenAt}, nil
}

func freezeProductionPrivilegeLogToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := freezeProductionPrivilegeLog(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, freezeProductionPrivilegeLogToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
