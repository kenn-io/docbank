package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var errProductionPrivilegeOutcomeUnknown = errors.New("production privilege mutation outcome is unknown")

type productionPrivilegeValidationOutput struct {
	privateCache

	DraftGeneration int64  `json:"draft_generation"`
	InputsSHA256    string `json:"inputs_sha256"`
	RowsSHA256      string `json:"rows_sha256"`
	ValidatedAt     string `json:"validated_at"`
}

func validateProductionPrivilegeLog(ctx context.Context, lease *daemonLease, raw []byte) (productionPrivilegeValidationOutput, error) {
	if len(raw) > 4096 {
		return productionPrivilegeValidationOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		LogID              string `json:"log_id"`
		Revision           int64  `json:"revision"`
		OperationID        string `json:"operation_id"`
		ExpectedGeneration int64  `json:"expected_generation"`
		ValidatedAt        string `json:"validated_at"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPrivilegeValidationOutput{}, err
	}
	if !validToolUUID(input.LogID) || input.Revision < 1 || !validToolUUID(input.OperationID) ||
		input.ExpectedGeneration < 1 || len(input.ValidatedAt) > 64 {
		return productionPrivilegeValidationOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.ProductionPrivilegeValidation, error) {
		return c.ValidateProductionPrivilegeLog(ctx, input.LogID, input.Revision,
			api.ProductionPrivilegeValidationRequest{OperationID: input.OperationID,
				ExpectedGeneration: input.ExpectedGeneration, ValidatedAt: input.ValidatedAt})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPrivilegeValidationOutput{}, errProductionPrivilegeOutcomeUnknown
		}
		return productionPrivilegeValidationOutput{}, err
	}
	return productionPrivilegeValidationOutput{privateCache: newPrivateCache(),
		DraftGeneration: result.DraftGeneration, InputsSHA256: result.Validation.InputsSHA256,
		RowsSHA256: result.Validation.RowsSHA256, ValidatedAt: result.Validation.Inputs.ValidatedAt}, nil
}

func validateProductionPrivilegeLogToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := validateProductionPrivilegeLog(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, validateProductionPrivilegeLogToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
