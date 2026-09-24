package mcp

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type findProductionNumbersInput struct {
	Label         string `json:"label"`
	NamespaceID   string `json:"namespace_id"`
	StartSequence int64  `json:"start_sequence"`
	EndSequence   int64  `json:"end_sequence"`
	AfterSequence int64  `json:"after_sequence"`
	Limit         int    `json:"limit"`
}

type productionNumberPageOutput struct {
	privateCache

	api.ProductionNumberPage
}

func findProductionNumbers(ctx context.Context, lease *daemonLease, raw []byte) (productionNumberPageOutput, error) {
	var input findProductionNumbersInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionNumberPageOutput{}, err
	}
	ranged := input.NamespaceID != "" || input.StartSequence != 0 || input.EndSequence != 0 ||
		input.AfterSequence != 0
	if (input.Label == "") == !ranged || input.Label != "" && input.Limit != 0 ||
		len(input.Label) > 256 || input.Limit < 0 || input.Limit > 25 ||
		ranged && (!validToolUUID(input.NamespaceID) || input.StartSequence < 1 ||
			input.EndSequence < input.StartSequence || input.AfterSequence < 0 ||
			input.AfterSequence > input.EndSequence) {
		return productionNumberPageOutput{}, invalidToolArgumentsError()
	}
	limit := input.Limit
	if limit == 0 {
		limit = 25
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.ProductionNumberPage, error) {
		result, err := c.ProductionNumbers(ctx, daemonconn.ProductionNumberQuery{
			Label: input.Label, NamespaceID: input.NamespaceID, StartSequence: input.StartSequence,
			EndSequence: input.EndSequence, AfterSequence: input.AfterSequence, Limit: input.Limit})
		return &result, err
	})
	if err != nil {
		return productionNumberPageOutput{}, err
	}
	if len(page.Items) > limit {
		return productionNumberPageOutput{}, errors.New("production number page exceeded its requested bound")
	}
	if page.Items == nil {
		page.Items = []api.ProductionNumberReference{}
	}
	return productionNumberPageOutput{ProductionNumberPage: *page, privateCache: newPrivateCache()}, nil
}
