package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionReceiptOutput struct {
	privateCache
	redaction.Receipt
}

func productionReviewWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output productionReceiptOutput
		var err error
		switch name {
		case editProductionInstructionsToolDefinition.name:
			output, err = editProductionInstructions(ctx, lease, request.Params.Arguments)
		case sealProductionMembershipToolDefinition.name:
			output, err = sealProductionMembership(ctx, lease, request.Params.Arguments)
		case reviewProductionMemberToolDefinition.name:
			output, err = reviewProductionMember(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown production review write tool")
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

func validProductionMutationFence(setID string, revision, etag int64, operationID string) bool {
	return validToolUUID(setID) && revision > 0 && etag > 0 && validToolUUID(operationID)
}

func editProductionInstructions(ctx context.Context, lease *daemonLease, raw []byte) (productionReceiptOutput, error) {
	var input struct {
		SetID        string `json:"set_id"`
		Revision     int64  `json:"revision"`
		ETag         int64  `json:"etag"`
		OperationID  string `json:"operation_id"`
		Instructions string `json:"instructions"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReceiptOutput{}, err
	}
	request := api.ProductionInstructionsRequest{OperationID: input.OperationID, Instructions: input.Instructions}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		redaction.ValidateInstructionsEditRequest(request.Domain(input.ETag)) != nil {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	receipt, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Receipt, error) {
		return c.EditProductionInstructions(ctx, input.SetID, input.Revision, input.ETag, request)
	})
	if err != nil {
		return productionReceiptOutput{}, err
	}
	return productionReceiptOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}

func sealProductionMembership(ctx context.Context, lease *daemonLease, raw []byte) (productionReceiptOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
		Total       int    `json:"total"`
		MemberHash  string `json:"member_hash"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReceiptOutput{}, err
	}
	request := api.ProductionMembershipSealRequest{OperationID: input.OperationID,
		Total: input.Total, MemberHash: input.MemberHash}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		redaction.ValidateMembershipSealRequest(request.Domain(input.ETag)) != nil {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	receipt, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Receipt, error) {
		return c.SealProductionMembership(ctx, input.SetID, input.Revision, input.ETag, request)
	})
	if err != nil {
		return productionReceiptOutput{}, err
	}
	return productionReceiptOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}

func reviewProductionMember(ctx context.Context, lease *daemonLease, raw []byte) (productionReceiptOutput, error) {
	var input struct {
		SetID       string `json:"set_id"`
		Revision    int64  `json:"revision"`
		ETag        int64  `json:"etag"`
		OperationID string `json:"operation_id"`
		MemberID    string `json:"member_id"`
		Binding     string `json:"binding"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionReceiptOutput{}, err
	}
	if !validProductionMutationFence(input.SetID, input.Revision, input.ETag, input.OperationID) ||
		!validToolUUID(input.MemberID) || !canonical.IsSHA256Hex(input.Binding) {
		return productionReceiptOutput{}, invalidToolArgumentsError()
	}
	request := api.ProductionMemberReviewRequest{OperationID: input.OperationID,
		Binding: input.Binding, Complete: true}
	receipt, err := productionWrite(ctx, lease, func(c *daemonconn.Connection) (redaction.Receipt, error) {
		return c.ReviewProductionMember(ctx, input.SetID, input.Revision, input.ETag, input.MemberID, request)
	})
	if err != nil {
		return productionReceiptOutput{}, err
	}
	return productionReceiptOutput{privateCache: newPrivateCache(), Receipt: receipt}, nil
}
