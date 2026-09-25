package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func productionChangesWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output productionReceiptOutput
		var err error
		switch name {
		case appendProductionMembersToolDefinition.name:
			output, err = appendProductionMembers(ctx, lease, request.Params.Arguments)
		case applyProductionChangesToolDefinition.name:
			output, err = applyProductionChanges(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown production changes write tool")
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

func appendProductionMembers(ctx context.Context, lease *daemonLease, raw []byte) (productionReceiptOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
		MembersJSON string `json:"members_json"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReceiptOutput{}, err
	}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		len(input.MembersJSON) > redaction.MaxCommandBytes {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	var members []api.ProductionMember
	if err := json.Unmarshal([]byte(input.MembersJSON), &members, json.RejectUnknownMembers(true)); err != nil {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionMemberAppendRequest{OperationID: input.OperationID, Members: members}
	if redaction.ValidateApplyRequest(request.Domain(input.ETag)) != nil {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > redaction.MaxCommandBytes {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	receipt, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Receipt, error) {
		return c.AppendProductionMembers(ctx, input.SetID, input.Revision, input.ETag, request)
	})
	if err != nil {
		return productionReceiptOutput{}, err
	}
	return productionReceiptOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}

func applyProductionChanges(ctx context.Context, lease *daemonLease, raw []byte) (productionReceiptOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
		ChangesJSON string `json:"changes_json"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReceiptOutput{}, err
	}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		len(input.ChangesJSON) > redaction.MaxCommandBytes {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	var changes []api.ProductionChange
	if err := json.Unmarshal([]byte(input.ChangesJSON), &changes, json.RejectUnknownMembers(true)); err != nil {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionChangesRequest{OperationID: input.OperationID, Changes: changes}
	if redaction.ValidateApplyRequest(request.Domain(input.ETag)) != nil {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > redaction.MaxCommandBytes {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	receipt, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Receipt, error) {
		return c.ApplyProductionChanges(ctx, input.SetID, input.Revision, input.ETag, request)
	})
	if err != nil {
		return productionReceiptOutput{}, err
	}
	return productionReceiptOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}
