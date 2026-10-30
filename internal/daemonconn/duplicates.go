package daemonconn

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func (c *Connection) DOCXDuplicates(ctx context.Context, limit, offset int) (api.DOCXDuplicatePage, error) {
	page, err := c.API().ListDOCXDuplicates(ctx, &apiclient.ListDOCXDuplicatesRequestOptions{
		Query: &apiclient.ListDOCXDuplicatesQuery{Limit: new(int64(limit)), Offset: new(int64(offset))},
	})
	if err != nil {
		return api.DOCXDuplicatePage{}, err
	}
	return *page, nil
}
