package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/production"
	"uuid"
)

// CreateProductionPackage builds and retains one exact recipient package
// through the daemon and verifies its public evidence receipt.
func (c *Connection) CreateProductionPackage(ctx context.Context, jobID string,
	input api.ProductionPackageCreateRequest) (production.PackageEvidenceReceipt, error) {
	if !validUUIDv4(jobID) || !validUUIDv4(input.OperationID) || input.ProfileID == "" ||
		input.MaxVolumeBytes < 1 || input.MaxVolumeBytes > 50<<30 ||
		input.MaxVolumeDocuments < 1 || input.MaxVolumeDocuments > 100_000 {
		return production.PackageEvidenceReceipt{}, errors.New("production package requires job and operation UUIDv4 identities, profile and bounded volume limits")
	}
	result, err := c.API().CreateProductionPackage(ctx, &apiclient.CreateProductionPackageRequestOptions{
		PathParams: &apiclient.CreateProductionPackagePath{JobID: uuid.MustParse(jobID)},
		Body:       &input,
	})
	if err != nil {
		return production.PackageEvidenceReceipt{}, err
	}
	if result == nil {
		return production.PackageEvidenceReceipt{}, integrityErrorf("production package evidence is missing")
	}
	evidence := production.PackageEvidenceReceipt(*result)
	if production.ValidatePackageEvidenceReceipt(evidence) != nil || evidence.ID != input.OperationID ||
		evidence.JobID != jobID || evidence.ProfileID != input.ProfileID {
		return production.PackageEvidenceReceipt{}, integrityErrorf("production package evidence disagrees with requested job, operation or profile")
	}
	return evidence, nil
}

// ProductionPackage reads and verifies one exact retained package evidence
// receipt after the daemon checks all three retained vault versions.
func (c *Connection) ProductionPackage(ctx context.Context, jobID, operationID string) (
	production.PackageEvidenceReceipt, error,
) {
	if !validUUIDv4(jobID) || !validUUIDv4(operationID) {
		return production.PackageEvidenceReceipt{}, errors.New("production package requires job and operation UUIDv4 identities")
	}
	result, err := c.API().GetProductionPackage(ctx, &apiclient.GetProductionPackageRequestOptions{
		PathParams: &apiclient.GetProductionPackagePath{
			JobID: uuid.MustParse(jobID), OperationID: uuid.MustParse(operationID),
		},
	})
	if err != nil {
		return production.PackageEvidenceReceipt{}, err
	}
	if result == nil {
		return production.PackageEvidenceReceipt{}, integrityErrorf("production package evidence is missing")
	}
	evidence := production.PackageEvidenceReceipt(*result)
	if production.ValidatePackageEvidenceReceipt(evidence) != nil || evidence.ID != operationID ||
		evidence.JobID != jobID {
		return production.PackageEvidenceReceipt{}, integrityErrorf("production package readback is inconsistent")
	}
	return evidence, nil
}
