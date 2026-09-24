package docbank

import (
	"context"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type ProductionMemberPage = api.ProductionMemberPage
type ProductionDecisionPage = api.ProductionDecisionPage
type ProductionSetPage = api.ProductionSetPage
type ProductionMapChunk = store.ProductionMapChunk
type ProductionJobStatus = store.ProductionJobStatus
type ProductionRecipeCatalog = api.ProductionRecipeCatalog

func (v *Vault) ProductionRecipes(ctx context.Context) (ProductionRecipeCatalog, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionRecipeCatalog{}, ErrClosed
	}
	return api.QualifiedProductionRecipes()
}

// CreateProductionSet creates an idempotent first draft in this embedded vault.
// Actor is the embedding application's authenticated principal.
func (v *Vault) CreateProductionSet(ctx context.Context, actor string, request redaction.CreateRequest) (redaction.Set, redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Set{}, redaction.Draft{}, ErrClosed
	}
	var set redaction.Set
	var draft redaction.Draft
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		set, draft, err = v.metadata.CreateProductionSet(ctx, actor, request)
		return err
	})
	return set, draft, err
}

func (v *Vault) ProductionSet(ctx context.Context, setID string) (redaction.Set, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Set{}, ErrClosed
	}
	return v.metadata.ProductionSet(ctx, setID)
}

func (v *Vault) ProductionSets(ctx context.Context, cursor string, limit int) (ProductionSetPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionSetPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ListProductionSets(ctx, cursor, limit)
	if err != nil {
		return ProductionSetPage{}, err
	}
	return ProductionSetPage{Items: items, NextCursor: next}, nil
}

func (v *Vault) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	return v.metadata.ProductionDraft(ctx, setID, revision)
}

// ForkProductionDraft copies one retained revision into a new editable draft.
// Review declarations and the membership seal are reset by the Store.
func (v *Vault) ForkProductionDraft(ctx context.Context, actor, setID string, revision int64,
	operationID string) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	var draft redaction.Draft
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		draft, err = v.metadata.ForkProductionDraft(ctx, actor, setID, revision, operationID)
		return err
	})
	return draft, err
}

func (v *Vault) ProductionMembers(ctx context.Context, setID string, revision int64, cursor string, limit int) (ProductionMemberPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionMemberPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionMembers(ctx, setID, revision, cursor, limit)
	if err != nil {
		return ProductionMemberPage{}, err
	}
	page := ProductionMemberPage{Items: make([]api.ProductionMember, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionMember(item)
	}
	return page, nil
}

func (v *Vault) ProductionDecisions(ctx context.Context, setID string, revision int64, cursor string, limit int) (ProductionDecisionPage, error) {
	return v.ProductionDecisionsFiltered(ctx, setID, revision, cursor, limit, nil)
}

// ProductionDecisionsFiltered pages only decisions matching an optional
// uncertainty state. Filtered cursors cannot be reused for another view.
func (v *Vault) ProductionDecisionsFiltered(ctx context.Context, setID string, revision int64,
	cursor string, limit int, uncertain *bool) (ProductionDecisionPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionDecisionPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionDecisionsFiltered(ctx, setID, revision, cursor, limit, uncertain)
	if err != nil {
		return ProductionDecisionPage{}, err
	}
	page := ProductionDecisionPage{Items: make([]api.ProductionDecision, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionDecision(item)
	}
	return page, nil
}

// ProductionMapChunk reads one bounded page of a retained member map from
// this embedded vault. The caller verifies the assembled map digest.
func (v *Vault) ProductionMapChunk(ctx context.Context, setID string, revision int64, memberID, cursor string, limit int) (ProductionMapChunk, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionMapChunk{}, ErrClosed
	}
	if limit == 0 {
		limit = store.MaxProductionMapChunkBytes
	}
	return v.metadata.ProductionMapChunk(ctx, setID, revision, memberID, cursor, limit)
}

func (v *Vault) ProductionJobStatus(ctx context.Context, setID, jobID string) (ProductionJobStatus, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionJobStatus{}, ErrClosed
	}
	return v.metadata.ProductionJobStatus(ctx, setID, jobID)
}

// AdmitProductionJob pins a finalized revision and enqueues one replay-safe job.
func (v *Vault) AdmitProductionJob(ctx context.Context, setID string, revision, etag int64,
	request api.ProductionJobAdmissionRequest) (ProductionJobStatus, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionJobStatus{}, ErrClosed
	}
	var status ProductionJobStatus
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		if _, err := v.metadata.AdmitFinalizedProductionJob(ctx, setID, revision, etag,
			request.JobID, request.OperationID); err != nil {
			return err
		}
		var err error
		status, err = v.metadata.ProductionJobStatus(ctx, setID, request.JobID)
		return err
	})
	return status, err
}

// CancelProductionJob records a replay-safe cancellation in this embedded vault.
func (v *Vault) CancelProductionJob(ctx context.Context, actor, setID, jobID string, etag int64,
	request api.ProductionJobCancelRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.CancelProductionJobOperation(ctx, actor, setID, jobID, etag, request.OperationID)
		return err
	})
	return receipt, err
}

func (v *Vault) EditProductionInstructions(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionInstructionsRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.EditProductionInstructions(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) ApplyProductionChanges(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionChangesRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.ApplyProductionChanges(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) SealProductionMembership(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionMembershipSealRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.SealProductionMembership(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) ReviewProductionMember(ctx context.Context, actor, setID string, revision, etag int64,
	memberID string, request api.ProductionMemberReviewRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.ReviewProductionMember(ctx, actor, setID, revision, request.Domain(etag, memberID))
		return err
	})
	return receipt, err
}
