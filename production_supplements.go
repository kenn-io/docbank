package docbank

import (
	"context"

	"go.kenn.io/docbank/internal/production"
)

type ProductionSupplementRequest = production.SupplementRequest
type ProductionSupplementRecord = production.SupplementRecord

// CreateProductionSupplement reserves continuation numbers for a prepared
// child and records its link to a published parent in this embedded vault.
// Actor is the embedding application's authenticated principal.
func (v *Vault) CreateProductionSupplement(ctx context.Context, actor string,
	request ProductionSupplementRequest) (ProductionSupplementRecord, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionSupplementRecord{}, ErrClosed
	}
	var record ProductionSupplementRecord
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		record, err = v.metadata.CreateProductionSupplement(ctx, actor, request)
		return err
	})
	return record, err
}

// ProductionSupplement reads a verified immutable parent-child continuation.
func (v *Vault) ProductionSupplement(ctx context.Context, operationID string) (ProductionSupplementRecord, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionSupplementRecord{}, ErrClosed
	}
	return v.metadata.LoadProductionSupplement(ctx, operationID)
}
