package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

type ProductionNumberQuery struct {
	Label         string
	NamespaceID   string
	StartSequence int64
	EndSequence   int64
	AfterSequence int64
	Limit         int
}

// ProductionNumbers reads exact or ranged published production number
// references through the authenticated daemon API.
func (c *Connection) ProductionNumbers(ctx context.Context, query ProductionNumberQuery) (api.ProductionNumberPage, error) {
	ranged := query.NamespaceID != "" || query.StartSequence != 0 || query.EndSequence != 0 ||
		query.AfterSequence != 0
	if (query.Label == "") == !ranged || query.Label != "" && query.Limit != 0 ||
		query.Limit < 0 || query.Limit > 25 {
		return api.ProductionNumberPage{}, errors.New("exact label or production number range is required")
	}
	request := apiclient.FindProductionNumbersQuery{}
	if query.Label != "" {
		request.Label = &query.Label
	} else {
		request.NamespaceID = &query.NamespaceID
		request.StartSequence = &query.StartSequence
		request.EndSequence = &query.EndSequence
		if query.AfterSequence != 0 {
			request.AfterSequence = &query.AfterSequence
		}
		if query.Limit != 0 {
			limit := int64(query.Limit)
			request.Limit = &limit
		}
	}
	result, err := c.API().FindProductionNumbers(ctx, &apiclient.FindProductionNumbersRequestOptions{Query: &request})
	if err != nil {
		return api.ProductionNumberPage{}, err
	}
	if result == nil || len(result.Items) > 25 || query.Label != "" &&
		(len(result.Items) != 1 || result.Items[0].Label != query.Label || result.NextSequence != 0) {
		return api.ProductionNumberPage{}, integrityErrorf("production number response is inconsistent")
	}
	return *result, nil
}
