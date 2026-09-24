package docbank

import (
	"context"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

type ProductionMemberPage = api.ProductionMemberPage
type ProductionDecisionPage = api.ProductionDecisionPage
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

func (v *Vault) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	return v.metadata.ProductionDraft(ctx, setID, revision)
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
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionDecisionPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionDecisions(ctx, setID, revision, cursor, limit)
	if err != nil {
		return ProductionDecisionPage{}, err
	}
	page := ProductionDecisionPage{Items: make([]api.ProductionDecision, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionDecision(item)
	}
	return page, nil
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
