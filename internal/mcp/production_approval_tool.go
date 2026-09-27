package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionApprovalOutput struct {
	privateCache
	api.ProductionApprovalPublic
}

func getProductionApproval(ctx context.Context, lease *daemonLease, raw []byte) (productionApprovalOutput, error) {
	var input struct {
		ApprovalID string `json:"approval_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionApprovalOutput{}, err
	}
	if !validToolUUID(input.ApprovalID) {
		return productionApprovalOutput{}, invalidToolArgumentsError()
	}
	public, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionApprovalPublic, error) {
		return c.ProductionApproval(ctx, input.ApprovalID)
	})
	if err != nil {
		return productionApprovalOutput{}, err
	}
	return productionApprovalOutput{privateCache: newPrivateCache(), ProductionApprovalPublic: public}, nil
}
