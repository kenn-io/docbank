package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

var errProductionSupplementOutcomeUnknown = errors.New("production supplement write outcome is unknown")

type productionSupplementOutput struct {
	privateCache

	Record production.SupplementRecord `json:"record"`
}

func getProductionSupplement(ctx context.Context, lease *daemonLease, raw []byte) (productionSupplementOutput, error) {
	var input struct {
		OperationID string `json:"operation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionSupplementOutput{}, err
	}
	if !validToolUUID(input.OperationID) {
		return productionSupplementOutput{}, invalidToolArgumentsError()
	}
	record, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (production.SupplementRecord, error) {
		return c.ProductionSupplement(ctx, input.OperationID)
	})
	if err != nil {
		return productionSupplementOutput{}, err
	}
	return productionSupplementOutput{privateCache: newPrivateCache(), Record: record}, nil
}

func createProductionSupplement(ctx context.Context, lease *daemonLease, raw []byte) (productionSupplementOutput, error) {
	if len(raw) > 4096 {
		return productionSupplementOutput{}, invalidToolArgumentsError()
	}
	var request production.SupplementRequest
	if err := decodeReadArguments(raw, &request); err != nil {
		return productionSupplementOutput{}, err
	}
	if _, err := production.SupplementRequestSHA256(request); err != nil {
		return productionSupplementOutput{}, invalidToolArgumentsError()
	}
	record, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (production.SupplementRecord, error) {
		return c.CreateProductionSupplement(ctx, request)
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionSupplementOutput{}, errProductionSupplementOutcomeUnknown
		}
		return productionSupplementOutput{}, err
	}
	return productionSupplementOutput{privateCache: newPrivateCache(), Record: record}, nil
}

func createProductionSupplementToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionSupplement(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionSupplementToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
