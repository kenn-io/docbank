package daemonconn

import (
	"context"
	"encoding/hex"
	"errors"

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
func (c *Connection) DeleteMissLog(ctx context.Context, before string) error {
	query := &apiclient.DeleteMissLogQuery{}
	if before != "" {
		query.Before = new(before)
	}
	_, err := c.API().DeleteMissLog(ctx, &apiclient.DeleteMissLogRequestOptions{Query: query})
	return err
}
func (c *Connection) CompleteEmptySearch(ctx context.Context, input api.EmptySearchCompletionRequest) (api.EmptySearchCompletion, error) {
	out, err := c.API().CompleteEmptySearch(ctx, &apiclient.CompleteEmptySearchRequestOptions{Body: &input})
	if err != nil {
		return api.EmptySearchCompletion{}, err
	}
	if !validMissLogID(out.MissLogID) {
		return api.EmptySearchCompletion{}, errors.New("search completion returned an invalid miss log ID")
	}
	return *out, nil
}

func validMissLogID(id string) bool {
	if id == "" {
		return true
	}
	if len(id) != 48 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && hex.EncodeToString(decoded) == id
}
