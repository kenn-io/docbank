package daemonconn

import (
	"context"
	"errors"
	"strconv"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// ProductionPrivilegeLog reads a bounded public page from an exact frozen log.
func (c *Connection) ProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	cursor string, limit int) (api.ProductionPrivilegePublicPage, error) {
	if logID == "" || revision < 1 || len(cursor) > 6 || limit < 0 || limit > store.MaxProductionPrivilegePage {
		return api.ProductionPrivilegePublicPage{}, errors.New("invalid production privilege page")
	}
	if limit == 0 {
		limit = 25
	}
	query := apiclient.ReadProductionPrivilegeLogQuery{Revision: &revision}
	if cursor != "" {
		query.Cursor = &cursor
	}
	requested := int64(limit)
	query.Limit = &requested
	result, err := c.API().ReadProductionPrivilegeLog(ctx,
		&apiclient.ReadProductionPrivilegeLogRequestOptions{
			PathParams: &apiclient.ReadProductionPrivilegeLogPath{Log: logID}, Query: &query,
		})
	if err != nil {
		return api.ProductionPrivilegePublicPage{}, err
	}
	if result == nil || result.Receipt.LogID != logID || result.Receipt.Revision != revision ||
		documentproduction.ValidatePrivilegeLogReceipt(result.Receipt) != nil || len(result.Rows) > limit {
		return api.ProductionPrivilegePublicPage{}, integrityErrorf("production privilege page is inconsistent")
	}
	if result.NextCursor != "" {
		position, err := strconv.Atoi(result.NextCursor)
		if err != nil || position < 1 || position > result.Receipt.RowCount ||
			strconv.Itoa(position) != result.NextCursor || len(result.Rows) != limit {
			return api.ProductionPrivilegePublicPage{}, integrityErrorf("production privilege cursor is inconsistent")
		}
	}
	return *result, nil
}
