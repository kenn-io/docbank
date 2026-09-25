package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var errProductionOutcomeUnknown = errors.New("production write outcome unknown; retry only with the same operation ID")

type productionSetCreatedOutput struct {
	privateCache
	api.ProductionSetCreated
}

func productionDraftWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case createProductionSetToolDefinition.name:
			output, err = createProductionSet(ctx, lease, request.Params.Arguments)
		case forkProductionDraftToolDefinition.name:
			output, err = forkProductionDraft(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown production draft write tool")
		}
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}

func productionWrite[T any](ctx context.Context, lease *daemonLease,
	callback func(*daemonconn.Connection) (T, error)) (T, error) {
	result, err := daemonProcessingStart(ctx, lease, callback)
	if errors.Is(err, errProcessingOutcomeUnknown) {
		var zero T
		return zero, errProductionOutcomeUnknown
	}
	return result, err
}

func createProductionSet(ctx context.Context, lease *daemonLease, raw []byte) (productionSetCreatedOutput, error) {
	var input redaction.CreateRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionSetCreatedOutput{}, err
	}
	if err := redaction.ValidateCreateRequest(input); err != nil {
		return productionSetCreatedOutput{}, invalidToolArgumentsError()
	}
	created, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (api.ProductionSetCreated, error) {
		return c.CreateProductionSet(ctx, input)
	})
	if err != nil {
		return productionSetCreatedOutput{}, err
	}
	return productionSetCreatedOutput{privateCache: newPrivateCache(), ProductionSetCreated: created}, nil
}

func forkProductionDraft(ctx context.Context, lease *daemonLease, raw []byte) (productionDraftOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		OperationID string `json:"operation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionDraftOutput{}, err
	}
	if !validToolUUID(input.SetID) || !validToolUUID(input.OperationID) || input.Revision < 1 {
		return productionDraftOutput{}, invalidToolArgumentsError()
	}
	draft, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Draft, error) {
		return c.ForkProductionDraft(ctx, input.SetID, input.Revision,
			api.ProductionForkRequest{OperationID: input.OperationID})
	})
	if err != nil {
		return productionDraftOutput{}, err
	}
	return productionDraftOutput{privateCache: newPrivateCache(), Draft: draft}, nil
}
