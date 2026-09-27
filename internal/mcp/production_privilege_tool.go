package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

type productionPrivilegeOutput struct {
	privateCache
	api.ProductionPrivilegePublicPage
}

func getProductionPrivilegeLog(ctx context.Context, lease *daemonLease, raw []byte) (productionPrivilegeOutput, error) {
	var input struct {
		LogID    string `json:"log_id"`
		Revision int64  `json:"revision"`
		Cursor   string `json:"cursor"`
		Limit    int    `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPrivilegeOutput{}, err
	}
	if !validToolUUID(input.LogID) || input.Revision < 1 || len(input.Cursor) > 6 ||
		input.Limit < 0 || input.Limit > store.MaxProductionPrivilegePage {
		return productionPrivilegeOutput{}, invalidToolArgumentsError()
	}
	if input.Limit == 0 {
		input.Limit = 25
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionPrivilegePublicPage, error) {
		return c.ProductionPrivilegeLog(ctx, input.LogID, input.Revision, input.Cursor, input.Limit)
	})
	if err != nil {
		return productionPrivilegeOutput{}, err
	}
	return productionPrivilegeOutput{privateCache: newPrivateCache(), ProductionPrivilegePublicPage: page}, nil
}
