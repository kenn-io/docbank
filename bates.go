package docbank

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/pdfstamp"
	internalprocessing "go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// Bates errors let embedded callers distinguish ledger outcomes with errors.Is.
var (
	ErrBatesReservationConflict = store.ErrBatesReservationConflict
	ErrBatesOverflow            = store.ErrBatesOverflow
	ErrBatesPageCountMismatch   = store.ErrBatesPageCountMismatch
	ErrBatesPageLimit           = store.ErrBatesPageLimit
	ErrBatesLabelCollision      = store.ErrBatesLabelCollision
	ErrInvalidBatesRequest      = store.ErrInvalidBatesRequest
	ErrInvalidBatesCursor       = store.ErrInvalidBatesCursor
)

type BatesNamespaceRequest = api.BatesNamespaceRequest
type BatesNamespace = api.BatesNamespace
type BatesNamespacePage = api.BatesNamespacePage
type BatesPageInput = api.BatesPageInput
type BatesPageLabel = api.BatesPageLabel
type BatesPlanRequest = api.BatesPlanRequest
type BatesReserveRequest = api.BatesReserveRequest
type BatesPlan = api.BatesPlan
type BatesAllocation = api.BatesAllocation
type BatesRecipe = pdfstamp.Recipe
type BatesStampEngine = pdfstamp.EngineIdentity

// BatesRecipeContractV1 is the only stamp recipe contract PublishBatesExport accepts.
const BatesRecipeContractV1 = pdfstamp.RecipeContractV1

type BatesExport = api.BatesExport
type BatesExportPage = api.BatesExportPage
type BatesArtifactPage = api.BatesArtifactPage

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
	bound, err := api.BindBatesPlan(ctx, v.metadata, request)
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
// binds the reserved labels to the exact sealed snapshot pages. The recipe
// chooses the namespace and first number, so the reservation always matches
// what PublishBatesExport will stamp.
func (v *Vault) ReserveBatesRange(ctx context.Context, request BatesReserveRequest) (BatesAllocation, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesAllocation{}, ErrClosed
	}
	bound, err := api.BindBatesReservation(ctx, v.metadata, request)
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

// PublishBatesExport stamps the reserved pages with the reviewed recipe,
// verifies the PDF, and retains it. Retrying the same allocation and recipe
// returns the existing export.
func (v *Vault) PublishBatesExport(ctx context.Context, allocationID string, recipe BatesRecipe) (BatesExport, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesExport{}, ErrClosed
	}
	var artifact store.BatesArtifact
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		artifact, err = internalprocessing.PublishBatesExport(ctx, v.metadata, v.blobs, allocationID, recipe)
		return err
	})
	if err != nil {
		return BatesExport{}, err
	}
	return batesExportFromStore(artifact), nil
}

// BatesExport reads one verified export by its allocation ID.
func (v *Vault) BatesExport(ctx context.Context, allocationID string) (BatesExport, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesExport{}, ErrClosed
	}
	artifact, err := v.metadata.BatesArtifact(ctx, allocationID)
	if err != nil {
		return BatesExport{}, err
	}
	return batesExportFromStore(artifact), nil
}

// BatesExports lists verified export history oldest first. Pass the last
// returned artifact ID as after to read the next page.
func (v *Vault) BatesExports(ctx context.Context, after string, limit int) (BatesExportPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return BatesExportPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, err := v.metadata.BatesArtifacts(ctx, after, limit)
	if err != nil {
		return BatesExportPage{}, err
	}
	total, err := v.metadata.BatesArtifactCount(ctx)
	if err != nil {
		return BatesExportPage{}, err
	}
	page := BatesExportPage{Items: make([]BatesExport, len(items)), Total: total}
	for index, item := range items {
		page.Items[index] = batesExportFromStore(item)
	}
	if len(items) == limit {
		page.NextAfter = items[len(items)-1].ArtifactID
	}
	return page, nil
}

// ReadBatesExport returns the retained PDF bytes after checking them against
// the catalog's content hash.
func (v *Vault) ReadBatesExport(ctx context.Context, allocationID string) ([]byte, BatesExport, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return nil, BatesExport{}, ErrClosed
	}
	data, artifact, err := internalprocessing.ReadBatesExport(ctx, v.metadata, v.blobs, allocationID)
	if err != nil {
		return nil, BatesExport{}, err
	}
	return data, batesExportFromStore(artifact), nil
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

func batesExportFromStore(value store.BatesArtifact) BatesExport {
	pages := make([]BatesArtifactPage, len(value.Pages))
	for index, page := range value.Pages {
		pages[index] = BatesArtifactPage{Ordinal: page.Ordinal, OccurrenceID: page.OccurrenceID,
			SourceBlobSHA256: page.SourceBlobSHA256, SourcePage: page.SourcePage,
			OutputPage: page.OutputPage, Label: page.Label}
	}
	return BatesExport{ArtifactID: value.ArtifactID, AllocationID: value.AllocationID,
		BlobSHA256: value.BlobSHA256, Size: value.Size, MediaType: value.MediaType,
		PageCount: value.PageCount, RecipeSHA256: value.RecipeSHA256,
		ManifestSHA256: value.ManifestSHA256, State: value.State, CreatedAt: value.CreatedAt, Pages: pages}
}
