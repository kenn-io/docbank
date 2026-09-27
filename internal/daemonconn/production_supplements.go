package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/production"
	"uuid"
)

// CreateProductionSupplement reserves a child's continuation and verifies the
// returned immutable link against the exact request sent to the daemon.
func (c *Connection) CreateProductionSupplement(ctx context.Context,
	request production.SupplementRequest) (production.SupplementRecord, error) {
	requestSHA256, err := production.SupplementRequestSHA256(request)
	if err != nil {
		return production.SupplementRecord{}, err
	}
	result, err := c.API().CreateProductionSupplement(ctx, &apiclient.CreateProductionSupplementRequestOptions{
		Body: new(api.ProductionSupplementRequest(request)),
	})
	if err != nil {
		return production.SupplementRecord{}, err
	}
	if result == nil {
		return production.SupplementRecord{}, integrityErrorf("production supplement response is missing")
	}
	record := production.SupplementRecord(*result)
	if err := production.ValidateSupplementRecord(record); err != nil ||
		record.OperationID != request.OperationID || record.ParentJobID != request.ParentJobID ||
		record.JobID != request.JobID || record.ParentReceiptSHA256 != request.ParentReceiptSHA256 ||
		record.PreparedSHA256 != request.PreparedSHA256 ||
		record.PreparedInputSHA256 != request.PreparedInputSHA256 || record.RequestSHA256 != requestSHA256 {
		return production.SupplementRecord{}, integrityErrorf("production supplement disagrees with requested parent and child")
	}
	return record, nil
}

// ProductionSupplement reads and verifies one exact recorded continuation.
func (c *Connection) ProductionSupplement(ctx context.Context, operationID string) (production.SupplementRecord, error) {
	if !validUUIDv4(operationID) {
		return production.SupplementRecord{}, errors.New("production supplement operation ID must be a UUIDv4")
	}
	result, err := c.API().GetProductionSupplement(ctx, &apiclient.GetProductionSupplementRequestOptions{
		PathParams: &apiclient.GetProductionSupplementPath{OperationID: uuid.MustParse(operationID)},
	})
	if err != nil {
		return production.SupplementRecord{}, err
	}
	if result == nil {
		return production.SupplementRecord{}, integrityErrorf("production supplement response is missing")
	}
	record := production.SupplementRecord(*result)
	if err := production.ValidateSupplementRecord(record); err != nil || record.OperationID != operationID {
		return production.SupplementRecord{}, integrityErrorf("production supplement readback is inconsistent")
	}
	return record, nil
}
