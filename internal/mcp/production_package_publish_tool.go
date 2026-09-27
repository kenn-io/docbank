package mcp

import (
	"context"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionPackagePublishedOutput struct {
	privateCache
	api.ProductionPackagePublished
}

func publishProductionPackage(ctx context.Context, lease *daemonLease, raw []byte) (productionPackagePublishedOutput, error) {
	var input struct {
		JobID              string `json:"job_id"`
		OperationID        string `json:"operation_id"`
		ProfileID          string `json:"profile_id"`
		MaxVolumeBytes     int64  `json:"max_volume_bytes"`
		MaxVolumeDocuments int    `json:"max_volume_documents"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPackagePublishedOutput{}, err
	}
	request := api.ProductionPackagePublishRequest{OperationID: input.OperationID,
		ProfileID: input.ProfileID, MaxVolumeBytes: input.MaxVolumeBytes,
		MaxVolumeDocuments: input.MaxVolumeDocuments}
	if !validToolUUID(input.JobID) || !request.Valid() {
		return productionPackagePublishedOutput{}, invalidToolArgumentsError()
	}
	published, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (api.ProductionPackagePublished, error) {
		return c.PublishProductionPackage(ctx, input.JobID, request)
	})
	if err != nil {
		return productionPackagePublishedOutput{}, err
	}
	return productionPackagePublishedOutput{privateCache: newPrivateCache(), ProductionPackagePublished: published}, nil
}

func productionPackagePublishToolHandler(
	lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := publishProductionPackage(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, publishProductionPackageToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
