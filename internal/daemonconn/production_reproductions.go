package daemonconn

import (
	"context"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/production"
	"uuid"
)

// CreateProductionReproduction builds a new retained package from the exact
// published original and verifies the daemon's receipt against the request.
func (c *Connection) CreateProductionReproduction(ctx context.Context, jobID string,
	input api.ProductionReproductionCreateRequest) (documentproduction.ReproductionReceipt, error) {
	if !validUUIDv4(jobID) {
		return documentproduction.ReproductionReceipt{}, errors.New("reproduction requires a job UUIDv4 identity")
	}
	if _, _, err := documentproduction.CanonicalReproductionRequest(input.Request); err != nil {
		return documentproduction.ReproductionReceipt{}, err
	}
	policySHA256, err := production.PackageDeliveryPolicySHA256(
		production.PackageDeliveryPolicy(input.DeliveryPolicy))
	if err != nil {
		return documentproduction.ReproductionReceipt{}, err
	}
	if policySHA256 != input.Request.DeliveryPolicySHA256 {
		return documentproduction.ReproductionReceipt{}, errors.New("reproduction delivery policy disagrees with request")
	}
	result, err := c.API().CreateProductionReproduction(ctx, &apiclient.CreateProductionReproductionRequestOptions{
		PathParams: &apiclient.CreateProductionReproductionPath{JobID: uuid.MustParse(jobID)},
		Body:       &input,
	})
	if err != nil {
		return documentproduction.ReproductionReceipt{}, err
	}
	if result == nil || documentproduction.ValidateReproductionReceipt(*result) != nil ||
		result.ID != input.Request.OperationID ||
		result.OriginalProductionReceiptSHA256 != input.Request.OriginalProductionReceiptSHA256 ||
		result.DeliveryPolicySHA256 != input.Request.DeliveryPolicySHA256 ||
		result.NumberAllocationCount != 0 {
		return documentproduction.ReproductionReceipt{}, integrityErrorf("production reproduction disagrees with requested original and policy")
	}
	return *result, nil
}

// ProductionReproduction reads one exact verified reproduction receipt after
// the daemon checks its original job and separately retained package.
func (c *Connection) ProductionReproduction(ctx context.Context, jobID, operationID string) (
	documentproduction.ReproductionReceipt, error,
) {
	if !validUUIDv4(jobID) || !validUUIDv4(operationID) {
		return documentproduction.ReproductionReceipt{}, errors.New("reproduction requires job and operation UUIDv4 identities")
	}
	result, err := c.API().GetProductionReproduction(ctx, &apiclient.GetProductionReproductionRequestOptions{
		PathParams: &apiclient.GetProductionReproductionPath{
			JobID: uuid.MustParse(jobID), OperationID: uuid.MustParse(operationID),
		},
	})
	if err != nil {
		return documentproduction.ReproductionReceipt{}, err
	}
	if result == nil || documentproduction.ValidateReproductionReceipt(*result) != nil ||
		result.ID != operationID || result.NumberAllocationCount != 0 {
		return documentproduction.ReproductionReceipt{}, integrityErrorf("production reproduction receipt is inconsistent")
	}
	return *result, nil
}
