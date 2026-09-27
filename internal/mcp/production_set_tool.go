package mcp

import (
	"context"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionSetPageInput struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

type productionSetIDInput struct {
	SetID string `json:"set_id"`
}

type productionDraftIDInput struct {
	SetID    string `json:"set_id"`
	Revision int64  `json:"revision"`
}

type productionSetPageOutput struct {
	privateCache
	api.ProductionSetPage
}

type productionSetOutput struct {
	privateCache

	Set redaction.Set `json:"set"`
}

type productionDraftOutput struct {
	privateCache

	Draft redaction.Draft `json:"draft"`
}

func listProductionSets(ctx context.Context, lease *daemonLease, raw []byte) (productionSetPageOutput, error) {
	var input productionSetPageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionSetPageOutput{}, err
	}
	if input.Limit < 0 || input.Limit > redaction.MaxProductionPage || len(input.Cursor) > 2048 {
		return productionSetPageOutput{}, invalidToolArgumentsError()
	}
	limit := input.Limit
	if limit == 0 {
		limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionSetPage, error) {
		return c.ProductionSets(ctx, input.Cursor, limit)
	})
	if err != nil {
		return productionSetPageOutput{}, err
	}
	return productionSetPageOutput{privateCache: newPrivateCache(), ProductionSetPage: page}, nil
}

func getProductionSet(ctx context.Context, lease *daemonLease, raw []byte) (productionSetOutput, error) {
	var input productionSetIDInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionSetOutput{}, err
	}
	if !validToolUUID(input.SetID) {
		return productionSetOutput{}, invalidToolArgumentsError()
	}
	set, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (redaction.Set, error) {
		return c.ProductionSet(ctx, input.SetID)
	})
	if err != nil {
		return productionSetOutput{}, err
	}
	return productionSetOutput{privateCache: newPrivateCache(), Set: set}, nil
}

func getProductionDraft(ctx context.Context, lease *daemonLease, raw []byte) (productionDraftOutput, error) {
	var input productionDraftIDInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionDraftOutput{}, err
	}
	if !validToolUUID(input.SetID) || input.Revision < 1 {
		return productionDraftOutput{}, invalidToolArgumentsError()
	}
	draft, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (redaction.Draft, error) {
		return c.ProductionDraft(ctx, input.SetID, input.Revision)
	})
	if err != nil {
		return productionDraftOutput{}, err
	}
	return productionDraftOutput{privateCache: newPrivateCache(), Draft: draft}, nil
}
