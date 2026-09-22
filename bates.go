package docbank

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

const maxEmbeddedBatesLabels = 250

type BatesNamespaceRequest = api.BatesNamespaceRequest
type BatesNamespace = api.BatesNamespace
type BatesNamespacePage = api.BatesNamespacePage
type BatesPageInput = api.BatesPageInput
type BatesPageLabel = api.BatesPageLabel
type BatesPlanRequest = api.BatesPlanRequest
type BatesPlan = api.BatesPlan
type BatesAllocation = api.BatesAllocation

type BatesNamespaceListRequest struct {
	Cursor string
	Limit  int
}

// EnsureBatesNamespace creates or returns one namespace with an immutable
// prefix, suffix, and padding contract.
func (v *Vault) EnsureBatesNamespace(ctx context.Context, request BatesNamespaceRequest) (BatesNamespace, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesNamespace{}, ErrClosed
	}
	var namespace store.BatesNamespace
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		namespace, err = v.metadata.EnsureBatesNamespace(ctx, request.Prefix, request.Suffix, request.Padding)
		return err
	})
	return batesNamespaceFromStore(namespace), err
}

// BatesNamespaces returns one stable bounded page of label namespaces.
func (v *Vault) BatesNamespaces(ctx context.Context, request BatesNamespaceListRequest) (BatesNamespacePage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesNamespacePage{}, ErrClosed
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	items, total, next, err := v.metadata.BatesNamespaces(ctx, request.Cursor, limit)
	if err != nil {
		return BatesNamespacePage{}, err
	}
	page := BatesNamespacePage{Items: make([]BatesNamespace, len(items)), Total: total, NextCursor: next}
	for index, item := range items {
		page.Items[index] = batesNamespaceFromStore(item)
	}
	return page, nil
}

// PlanBatesStamp previews exact labels for a sealed snapshot without reserving
// a range or changing the namespace cursor.
func (v *Vault) PlanBatesStamp(ctx context.Context, request BatesPlanRequest) (BatesPlan, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesPlan{}, ErrClosed
	}
	bound, err := v.bindBatesPlan(ctx, request)
	if err != nil {
		return BatesPlan{}, err
	}
	plan, err := v.metadata.PreviewBatesRange(ctx, bound)
	if err != nil {
		return BatesPlan{}, err
	}
	return BatesPlan{Namespace: batesNamespaceFromStore(plan.Namespace), StartSequence: plan.StartSequence,
		EndSequence: plan.EndSequence, Labels: batesLabelsFromStore(plan.Labels), StampedNothing: true}, nil
}

// ReserveBatesRange advances a namespace once for an idempotent operation and
// binds the reserved labels to the exact sealed snapshot pages.
func (v *Vault) ReserveBatesRange(ctx context.Context, request BatesPlanRequest) (BatesAllocation, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesAllocation{}, ErrClosed
	}
	bound, err := v.bindBatesPlan(ctx, request)
	if err != nil {
		return BatesAllocation{}, err
	}
	var allocation store.BatesAllocation
	err = embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		allocation, err = v.metadata.ReserveBatesRange(ctx, bound)
		return err
	})
	return batesAllocationFromStore(allocation), err
}

// BatesAllocation reads one retained allocation and its page labels.
func (v *Vault) BatesAllocation(ctx context.Context, allocationID string) (BatesAllocation, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesAllocation{}, ErrClosed
	}
	allocation, err := v.metadata.BatesAllocation(ctx, allocationID)
	return batesAllocationFromStore(allocation), err
}

func (v *Vault) bindBatesPlan(ctx context.Context, request BatesPlanRequest) (store.BatesPlanRequest, error) {
	if len(request.Pages) > maxEmbeddedBatesLabels {
		return store.BatesPlanRequest{}, store.ErrBatesPageCountMismatch
	}
	namespace, err := v.metadata.BatesNamespace(ctx, request.NamespaceID, request.Prefix, request.Suffix, request.Padding)
	if err != nil {
		return store.BatesPlanRequest{}, err
	}
	pages := make([]store.BatesPageInput, len(request.Pages))
	for index, page := range request.Pages {
		pages[index] = store.BatesPageInput{OccurrenceID: page.OccurrenceID, UnstampedSHA256: page.UnstampedSHA256,
			SourcePage: page.SourcePage, VerifiedPageCount: page.VerifiedPageCount}
	}
	if len(pages) == 0 {
		pages, err = v.metadata.SnapshotBatesPages(ctx, request.SnapshotID, maxEmbeddedBatesLabels)
		if err != nil {
			return store.BatesPlanRequest{}, err
		}
	}
	return store.BatesPlanRequest{OperationID: request.OperationID, NamespaceID: namespace.NamespaceID,
		SnapshotID: request.SnapshotID, RecipeSHA256: request.RecipeSHA256, StartAt: request.StartAt, Pages: pages}, nil
}

func batesNamespaceFromStore(value store.BatesNamespace) BatesNamespace {
	return BatesNamespace{NamespaceID: value.NamespaceID, Prefix: value.Prefix, Suffix: value.Suffix,
		Padding: value.Padding, CreatedAt: value.CreatedAt}
}

func batesLabelsFromStore(values []store.BatesPageLabel) []BatesPageLabel {
	result := make([]BatesPageLabel, len(values))
	for index, value := range values {
		result[index] = BatesPageLabel{Ordinal: value.Ordinal, OccurrenceID: value.OccurrenceID,
			SourcePage: value.SourcePage, OutputPage: value.OutputPage, Label: value.Label}
	}
	return result
}

func batesAllocationFromStore(value store.BatesAllocation) BatesAllocation {
	return BatesAllocation{AllocationID: value.AllocationID, NamespaceID: value.NamespaceID,
		SnapshotID: value.SnapshotID, RecipeSHA256: value.RecipeSHA256, State: value.State,
		StartSequence: value.StartSequence, EndSequence: value.EndSequence,
		Labels: batesLabelsFromStore(value.Labels), CreatedAt: value.CreatedAt, CommittedAt: value.CommittedAt}
}
