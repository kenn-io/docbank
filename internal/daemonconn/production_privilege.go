package daemonconn

import (
	"context"
	"errors"
	"strconv"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// CreateProductionPrivilegeLogDraft submits private rows for one sealed
// production revision and returns a digest-free generation receipt.
func (c *Connection) CreateProductionPrivilegeLogDraft(ctx context.Context, logID string, revision int64,
	request api.ProductionPrivilegeDraftCreateRequest) (api.ProductionPrivilegeDraftGeneration, error) {
	if logID == "" || revision < 1 || request.OperationID == "" || request.SetID == "" ||
		request.SetRevision < 1 || len(request.PlayersSHA256) != 64 ||
		len(request.Rows) == 0 || len(request.Rows) > documentproduction.MaxPrivilegeRows {
		return api.ProductionPrivilegeDraftGeneration{}, errors.New("invalid production privilege draft request")
	}
	result, err := c.API().CreateProductionPrivilegeLogDraft(ctx,
		&apiclient.CreateProductionPrivilegeLogDraftRequestOptions{
			PathParams: &apiclient.CreateProductionPrivilegeLogDraftPath{Log: logID, Revision: revision},
			Body:       &request,
		})
	if err != nil {
		return api.ProductionPrivilegeDraftGeneration{}, err
	}
	if result == nil || result.LogID != logID || result.Revision != revision || result.Generation < 1 {
		return api.ProductionPrivilegeDraftGeneration{}, integrityErrorf("production privilege draft receipt is inconsistent")
	}
	return *result, nil
}

// ValidateProductionPrivilegeLog validates persisted draft authority through
// the daemon and returns only its digest summary.
func (c *Connection) ValidateProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	request api.ProductionPrivilegeValidationRequest) (api.ProductionPrivilegeValidation, error) {
	validatedAt, err := time.Parse(time.RFC3339Nano, request.ValidatedAt)
	if logID == "" || revision < 1 || request.OperationID == "" || request.ExpectedGeneration < 1 ||
		err != nil || validatedAt.Location() != time.UTC ||
		validatedAt.Format(time.RFC3339Nano) != request.ValidatedAt {
		return api.ProductionPrivilegeValidation{}, errors.New("invalid production privilege validation request")
	}
	result, err := c.API().ValidateProductionPrivilegeLog(ctx,
		&apiclient.ValidateProductionPrivilegeLogRequestOptions{
			PathParams: &apiclient.ValidateProductionPrivilegeLogPath{Log: logID, Revision: revision},
			Body:       &request,
		})
	if err != nil {
		return api.ProductionPrivilegeValidation{}, err
	}
	if result == nil || result.DraftGeneration != request.ExpectedGeneration ||
		result.Validation.Inputs.LogID != logID || result.Validation.Inputs.Revision != revision ||
		result.Validation.Inputs.ValidatedAt != request.ValidatedAt ||
		len(result.Validation.InputsSHA256) != 64 || len(result.Validation.RowsSHA256) != 64 {
		return api.ProductionPrivilegeValidation{}, integrityErrorf("production privilege validation is inconsistent")
	}
	return *result, nil
}

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
