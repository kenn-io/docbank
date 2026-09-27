package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

var errProductionReproductionOutcomeUnknown = errors.New("production reproduction write outcome is unknown")

type productionReproductionOutput struct {
	privateCache

	Receipt documentproduction.ReproductionReceipt `json:"receipt"`
}

func getProductionReproduction(ctx context.Context, lease *daemonLease,
	raw []byte) (productionReproductionOutput, error) {
	var input struct {
		JobID       string `json:"job_id"`
		OperationID string `json:"operation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReproductionOutput{}, err
	}
	if !validToolUUID(input.JobID) || !validToolUUID(input.OperationID) {
		return productionReproductionOutput{}, invalidToolArgumentsError()
	}
	receipt, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (
		documentproduction.ReproductionReceipt, error) {
		return c.ProductionReproduction(ctx, input.JobID, input.OperationID)
	})
	if err != nil {
		return productionReproductionOutput{}, err
	}
	return productionReproductionOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}

func createProductionReproduction(ctx context.Context, lease *daemonLease,
	raw []byte) (productionReproductionOutput, error) {
	if len(raw) > 1<<20 {
		return productionReproductionOutput{}, invalidToolArgumentsError()
	}
	var args struct {
		JobID              string                                   `json:"job_id"`
		Request            documentproduction.ReproductionRequest   `json:"request"`
		DeliveryPolicy     api.ProductionReproductionDeliveryPolicy `json:"delivery_policy"`
		ProfileID          string                                   `json:"profile_id"`
		MaxVolumeBytes     int64                                    `json:"max_volume_bytes"`
		MaxVolumeDocuments int                                      `json:"max_volume_documents"`
	}
	if err := decodeReadArguments(raw, &args); err != nil {
		return productionReproductionOutput{}, err
	}
	if !validToolUUID(args.JobID) || args.ProfileID == "" ||
		args.MaxVolumeBytes < 1 || args.MaxVolumeBytes > 50<<30 ||
		args.MaxVolumeDocuments < 1 || args.MaxVolumeDocuments > 100_000 {
		return productionReproductionOutput{}, invalidToolArgumentsError()
	}
	policySHA256, err := production.PackageDeliveryPolicySHA256(
		production.PackageDeliveryPolicy(args.DeliveryPolicy))
	if err != nil || args.Request.DeliveryPolicySHA256 != "" &&
		args.Request.DeliveryPolicySHA256 != policySHA256 {
		return productionReproductionOutput{}, invalidToolArgumentsError()
	}
	args.Request.DeliveryPolicySHA256 = policySHA256
	if _, _, err := documentproduction.CanonicalReproductionRequest(args.Request); err != nil {
		return productionReproductionOutput{}, invalidToolArgumentsError()
	}
	input := api.ProductionReproductionCreateRequest{
		Request: args.Request, DeliveryPolicy: args.DeliveryPolicy,
		ProfileID: args.ProfileID, MaxVolumeBytes: args.MaxVolumeBytes,
		MaxVolumeDocuments: args.MaxVolumeDocuments,
	}
	receipt, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (
		documentproduction.ReproductionReceipt, error) {
		return c.CreateProductionReproduction(ctx, args.JobID, input)
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionReproductionOutput{}, errProductionReproductionOutcomeUnknown
		}
		return productionReproductionOutput{}, err
	}
	return productionReproductionOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}

func createProductionReproductionToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionReproduction(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionReproductionToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
