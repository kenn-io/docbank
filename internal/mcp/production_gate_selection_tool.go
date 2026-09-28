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

var errProductionGateSelectionOutcomeUnknown = errors.New("production gate selection outcome is unknown")

type productionGateSelectionOutput struct {
	privateCache

	SetID                string `json:"set_id"`
	Revision             int64  `json:"revision"`
	ApprovalID           string `json:"approval_id,omitzero"`
	PrivilegeLogID       string `json:"privilege_log_id,omitzero"`
	PrivilegeLogRevision int64  `json:"privilege_log_revision,omitzero"`
}

func selectProductionGateAuthority(ctx context.Context, lease *daemonLease, raw []byte) (productionGateSelectionOutput, error) {
	if len(raw) > 4096 {
		return productionGateSelectionOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		SetID                string `json:"set_id"`
		Revision             int64  `json:"revision"`
		ApprovalID           string `json:"approval_id"`
		PrivilegeLogID       string `json:"privilege_log_id"`
		PrivilegeLogRevision int64  `json:"privilege_log_revision"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionGateSelectionOutput{}, err
	}
	if !validToolUUID(input.SetID) || input.Revision < 1 ||
		input.ApprovalID != "" && !validToolUUID(input.ApprovalID) ||
		(input.PrivilegeLogID == "") != (input.PrivilegeLogRevision == 0) ||
		input.PrivilegeLogID != "" && (!validToolUUID(input.PrivilegeLogID) || input.PrivilegeLogRevision < 1) {
		return productionGateSelectionOutput{}, invalidToolArgumentsError()
	}
	err := daemonProcessingStartVoid(ctx, lease, func(c *daemonconn.Connection) error {
		return c.SelectProductionGateAuthority(ctx, input.SetID, input.Revision,
			api.ProductionGateSelectionRequest{ApprovalID: input.ApprovalID,
				PrivilegeLogID: input.PrivilegeLogID, PrivilegeLogRevision: input.PrivilegeLogRevision})
	})
	if errors.Is(err, errProcessingOutcomeUnknown) {
		return productionGateSelectionOutput{}, errProductionGateSelectionOutcomeUnknown
	}
	if err != nil {
		return productionGateSelectionOutput{}, err
	}
	return productionGateSelectionOutput{privateCache: newPrivateCache(), SetID: input.SetID,
		Revision: input.Revision, ApprovalID: input.ApprovalID,
		PrivilegeLogID: input.PrivilegeLogID, PrivilegeLogRevision: input.PrivilegeLogRevision}, nil
}

func selectProductionGateAuthorityToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := selectProductionGateAuthority(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, selectProductionGateAuthorityToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
