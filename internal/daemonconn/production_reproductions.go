package daemonconn

import (
	"context"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/apiclient"
	"uuid"
)

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
