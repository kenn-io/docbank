package daemonconn

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/misslog"
)

func (c *Connection) ListMissLog(ctx context.Context, limit, offset int) (api.MissLogPage, error) {
	page, err := c.API().ListMissLog(ctx, &apiclient.ListMissLogRequestOptions{Query: &apiclient.ListMissLogQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	if err != nil {
		return api.MissLogPage{}, err
	}
	return *page, nil
}
func (c *Connection) MissLogRecord(ctx context.Context, id string) (misslog.Receipt, error) {
	record, err := c.API().GetMissLogRecord(ctx, &apiclient.GetMissLogRecordRequestOptions{PathParams: &apiclient.GetMissLogRecordPath{ID: id}})
	if err != nil {
		return misslog.Receipt{}, err
	}
	return *record, nil
}
func (c *Connection) DeleteMissLogRecord(ctx context.Context, id string) error {
	_, err := c.API().DeleteMissLogRecord(ctx, &apiclient.DeleteMissLogRecordRequestOptions{PathParams: &apiclient.DeleteMissLogRecordPath{ID: id}})
	return err
}
func (c *Connection) DeleteMissLog(ctx context.Context) error {
	_, err := c.API().DeleteMissLog(ctx)
	return err
}
