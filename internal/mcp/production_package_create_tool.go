package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

var errProductionPackageOutcomeUnknown = errors.New("production package write outcome is unknown")

type productionPackageEvidenceOutput struct {
	privateCache

	Evidence production.PackageEvidenceReceipt `json:"evidence"`
}

func getProductionPackage(ctx context.Context, lease *daemonLease, raw []byte) (productionPackageEvidenceOutput, error) {
	var input struct {
		JobID       string `json:"job_id"`
		OperationID string `json:"operation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPackageEvidenceOutput{}, err
	}
	if !validToolUUID(input.JobID) || !validToolUUID(input.OperationID) {
		return productionPackageEvidenceOutput{}, invalidToolArgumentsError()
	}
	evidence, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (
		production.PackageEvidenceReceipt, error) {
		return c.ProductionPackage(ctx, input.JobID, input.OperationID)
	})
	if err != nil {
		return productionPackageEvidenceOutput{}, err
	}
	return productionPackageEvidenceOutput{privateCache: newPrivateCache(), Evidence: evidence}, nil
}

func createProductionPackage(ctx context.Context, lease *daemonLease, raw []byte) (productionPackageEvidenceOutput, error) {
	if len(raw) > 1<<20 {
		return productionPackageEvidenceOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		JobID              string `json:"job_id"`
		OperationID        string `json:"operation_id"`
		ProfileID          string `json:"profile_id"`
		MaxVolumeBytes     int64  `json:"max_volume_bytes"`
		MaxVolumeDocuments int    `json:"max_volume_documents"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPackageEvidenceOutput{}, err
	}
	if !validToolUUID(input.JobID) || !validToolUUID(input.OperationID) || input.ProfileID == "" ||
		input.MaxVolumeBytes < 1 || input.MaxVolumeBytes > 50<<30 ||
		input.MaxVolumeDocuments < 1 || input.MaxVolumeDocuments > 100_000 {
		return productionPackageEvidenceOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionPackageCreateRequest{OperationID: input.OperationID, ProfileID: input.ProfileID,
		MaxVolumeBytes: input.MaxVolumeBytes, MaxVolumeDocuments: input.MaxVolumeDocuments}
	evidence, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (
		production.PackageEvidenceReceipt, error) {
		return c.CreateProductionPackage(ctx, input.JobID, request)
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPackageEvidenceOutput{}, errProductionPackageOutcomeUnknown
		}
		return productionPackageEvidenceOutput{}, err
	}
	return productionPackageEvidenceOutput{privateCache: newPrivateCache(), Evidence: evidence}, nil
}

func createProductionPackageToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionPackage(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionPackageToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
