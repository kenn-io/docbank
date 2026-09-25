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

type productionFinalizationOutput struct {
	privateCache
	api.ProductionFinalizationResult
}

func productionJobWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case finalizeProductionDraftToolDefinition.name:
			output, err = finalizeProductionDraft(ctx, lease, request.Params.Arguments)
		case admitProductionJobToolDefinition.name:
			output, err = admitProductionJob(ctx, lease, request.Params.Arguments)
		case cancelProductionJobToolDefinition.name:
			output, err = cancelProductionJob(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown production job write tool")
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

func finalizeProductionDraft(ctx context.Context, lease *daemonLease, raw []byte) (productionFinalizationOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
		NamespaceID string `json:"namespace_id"`
		SnapshotID  string `json:"snapshot_id"`
		StartAt     int64  `json:"start_at"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionFinalizationOutput{}, err
	}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		!validToolUUID(input.NamespaceID) || !validToolUUID(input.SnapshotID) || input.StartAt < 0 {
		return productionFinalizationOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionFinalizeRequest{OperationID: input.OperationID,
		NamespaceID: input.NamespaceID, SnapshotID: input.SnapshotID, StartAt: input.StartAt}
	result, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (api.ProductionFinalizationResult, error) {
		return c.FinalizeProductionDraft(ctx, input.SetID, input.Revision, input.ETag, request)
	})
	if err != nil {
		return productionFinalizationOutput{}, err
	}
	return productionFinalizationOutput{privateCache: newPrivateCache(), ProductionFinalizationResult: result}, nil
}

func admitProductionJob(ctx context.Context, lease *daemonLease, raw []byte) (productionJobOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
		JobID       string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionJobOutput{}, err
	}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		!validToolUUID(input.JobID) {
		return productionJobOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionJobAdmissionRequest{JobID: input.JobID, OperationID: input.OperationID}
	status, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (api.ProductionJobStatus, error) {
		return c.AdmitProductionJob(ctx, input.SetID, input.Revision, input.ETag, request)
	})
	if err != nil {
		return productionJobOutput{}, err
	}
	return productionJobOutput{privateCache: newPrivateCache(), Job: status}, nil
}

func cancelProductionJob(ctx context.Context, lease *daemonLease, raw []byte) (productionReceiptOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		JobID       string `json:"job_id"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReceiptOutput{}, err
	}
	if !validToolUUID(input.SetID) || !validToolUUID(input.JobID) || input.ETag < 1 ||
		!validToolUUID(input.OperationID) {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionJobCancelRequest{OperationID: input.OperationID}
	receipt, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Receipt, error) {
		return c.CancelProductionJob(ctx, input.SetID, input.JobID, input.ETag, request)
	})
	if err != nil {
		return productionReceiptOutput{}, err
	}
	return productionReceiptOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}
