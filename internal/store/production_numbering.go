package store

import "context"

// ProductionNumberingRequest binds one durable production job to the shared
// Bates allocator. JobID is the allocator operation identity, so retries and
// recovery use the ledger's existing idempotency authority.
type ProductionNumberingRequest struct {
	JobID, NamespaceID, SnapshotID, RecipeSHA256 string
	StartAt                                      int64
	Pages                                        []BatesPageInput
}

// ReserveProductionNumbers reserves through the existing Bates ledger. It
// never maintains a separate production cursor or sequence.
func (s *Store) ReserveProductionNumbers(
	ctx context.Context,
	request ProductionNumberingRequest,
) (BatesAllocation, error) {
	return s.ReserveBatesRange(ctx, BatesPlanRequest{
		OperationID:  request.JobID,
		NamespaceID:  request.NamespaceID,
		SnapshotID:   request.SnapshotID,
		RecipeSHA256: request.RecipeSHA256,
		StartAt:      request.StartAt,
		Pages:        request.Pages,
	})
}

// ProductionNumberingForJob returns the exact allocation retained for a job.
func (s *Store) ProductionNumberingForJob(ctx context.Context, jobID string) (BatesAllocation, error) {
	return s.BatesAllocationForOperation(ctx, jobID)
}

// AbandonProductionNumbers retires a job reservation without making its
// numbers available to a later job.
func (s *Store) AbandonProductionNumbers(ctx context.Context, jobID string) (BatesAllocation, error) {
	allocation, err := s.BatesAllocationForOperation(ctx, jobID)
	if err != nil {
		return BatesAllocation{}, err
	}
	return s.AbandonBatesAllocation(ctx, allocation.AllocationID)
}
