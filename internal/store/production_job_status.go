package store

import (
	"context"

	"go.kenn.io/docbank/internal/production"
)

// ProductionJobStatus is the bounded operator view of a retained job.
// Publication internals remain in the private verified receipt.
type ProductionJobStatus struct {
	JobID          string `json:"job_id"`
	SetID          string `json:"set_id"`
	Revision       int64  `json:"revision"`
	State          string `json:"state"`
	RevisionSHA256 string `json:"revision_sha256"`
	ReceiptSHA256  string `json:"receipt_sha256,omitzero"`
}

func (s *Store) ProductionJobStatus(ctx context.Context, setID, jobID string) (ProductionJobStatus, error) {
	if validateUUIDv4(setID) != nil || validateUUIDv4(jobID) != nil {
		return ProductionJobStatus{}, ErrNotFound
	}
	job, err := s.LoadProductionJob(ctx, jobID)
	if err != nil {
		return ProductionJobStatus{}, err
	}
	if job.SetID != setID || job.Revision < 1 {
		return ProductionJobStatus{}, ErrNotFound
	}
	switch job.State {
	case production.ProductionJobQueued, production.ProductionJobRunning,
		production.ProductionJobFailed, production.ProductionJobCanceled:
		if job.Receipt.SHA256 != "" {
			return ProductionJobStatus{}, ErrInvalidProduction
		}
	case production.ProductionJobSucceeded:
		if job.Receipt.SHA256 == "" {
			return ProductionJobStatus{}, ErrInvalidProduction
		}
	default:
		return ProductionJobStatus{}, ErrInvalidProduction
	}
	return ProductionJobStatus{JobID: job.ID, SetID: job.SetID, Revision: job.Revision,
		State: job.State, RevisionSHA256: job.RevisionSHA256, ReceiptSHA256: job.Receipt.SHA256}, nil
}
