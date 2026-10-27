package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/misslog"
)

func (c *Connection) ExportMissLog(ctx context.Context, format string, limit, offset int) (misslog.ExportPage, error) {
	page, err := c.API().ExportMissLog(ctx, &apiclient.ExportMissLogRequestOptions{Query: &apiclient.ExportMissLogQuery{Format: new(apiclient.ExportMissLogQueryFormat(format)), Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return misslog.ExportPage{}, err
	}
	if page.NextOffset < offset || page.NextOffset > offset+limit || (!page.Done && page.NextOffset == offset) || len(page.Items) > limit {
		return misslog.ExportPage{}, errors.New("invalid miss log export page")
	}
	return *page, nil
}
