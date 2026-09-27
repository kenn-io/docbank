package mcp

import (
	"context"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionRecipeSummary struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	DPI    int    `json:"dpi"`
}

type productionRecipesOutput struct {
	privateCache

	DefaultID string                    `json:"default_id"`
	Items     []productionRecipeSummary `json:"items"`
}

type productionResolveInput struct {
	SetID    string `json:"set_id"`
	Revision int64  `json:"revision"`
	ETag     int64  `json:"etag"`
	MemberID string `json:"member_id"`
	Page     int    `json:"page"`
	Cursor   string `json:"cursor"`
	Limit    int    `json:"limit"`
}

type productionResolvedPage struct {
	Number      int    `json:"number"`
	FrameSHA256 string `json:"frame_sha256"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
}

type productionResolveOutput struct {
	privateCache

	SetID          string                 `json:"set_id"`
	Revision       int64                  `json:"revision"`
	ETag           int64                  `json:"etag"`
	MemberID       string                 `json:"member_id"`
	Page           productionResolvedPage `json:"page"`
	MapSHA256      string                 `json:"map_sha256"`
	RecipeSHA256   string                 `json:"recipe_sha256"`
	ResolvedSHA256 string                 `json:"resolved_sha256"`
	ReviewBinding  string                 `json:"review_binding"`
	TotalBoxes     int                    `json:"total_boxes"`
	Items          []redaction.Box        `json:"items"`
	NextCursor     string                 `json:"next_cursor"`
}

func listProductionRecipes(ctx context.Context, lease *daemonLease, raw []byte) (productionRecipesOutput, error) {
	var input struct{}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionRecipesOutput{}, err
	}
	catalog, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionRecipeCatalog, error) {
		return c.ProductionRecipes(ctx)
	})
	if err != nil {
		return productionRecipesOutput{}, err
	}
	output := productionRecipesOutput{privateCache: newPrivateCache(), DefaultID: catalog.DefaultID,
		Items: make([]productionRecipeSummary, 0, len(catalog.Items))}
	for _, item := range catalog.Items {
		output.Items = append(output.Items, productionRecipeSummary{ID: item.ID, SHA256: item.SHA256, DPI: item.Recipe.DPI})
	}
	return output, nil
}

func resolveProductionSelection(ctx context.Context, lease *daemonLease, raw []byte) (productionResolveOutput, error) {
	var input productionResolveInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionResolveOutput{}, err
	}
	if !validToolUUID(input.SetID) || !validToolUUID(input.MemberID) || input.Revision < 1 ||
		input.ETag < 1 || input.Page < 1 || input.Limit < 0 ||
		input.Limit > redaction.MaxProductionPage || len(input.Cursor) > 1024 {
		return productionResolveOutput{}, invalidToolArgumentsError()
	}
	limit := input.Limit
	if limit == 0 {
		limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionResolvedMaskPage, error) {
		return c.ResolveProductionSelection(ctx, input.SetID, input.Revision, input.ETag,
			api.ProductionResolveRequest{MemberID: input.MemberID, Page: input.Page, Cursor: input.Cursor, Limit: limit})
	})
	if err != nil {
		return productionResolveOutput{}, err
	}
	return productionResolveOutput{privateCache: newPrivateCache(),
		SetID: page.SetID, Revision: page.Revision, ETag: page.ETag, MemberID: page.MemberID,
		Page: productionResolvedPage{Number: page.Page.Number, FrameSHA256: page.Page.FrameSHA256,
			Width: page.Page.Width, Height: page.Page.Height},
		MapSHA256: page.MapSHA256, RecipeSHA256: page.RecipeSHA256,
		ResolvedSHA256: page.ResolvedSHA256, ReviewBinding: page.ReviewBinding,
		TotalBoxes: page.TotalBoxes, Items: page.Items, NextCursor: page.NextCursor}, nil
}
