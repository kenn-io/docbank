package docbank

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type ProductionNumberReference = api.ProductionNumberReference
type ProductionNumberPage = api.ProductionNumberPage
type ProductionNumberCandidates = api.ProductionNumberCandidates

type ProductionNumberQuery struct {
	Label         string
	NamespaceID   string
	StartSequence int64
	EndSequence   int64
	AfterSequence int64
	Limit         int
}

// ProductionNumbers resolves an exact published label or pages a bounded
// numeric range from this vault's retained production authority.
func (v *Vault) ProductionNumbers(ctx context.Context, query ProductionNumberQuery) (ProductionNumberPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionNumberPage{}, ErrClosed
	}
	ranged := query.NamespaceID != "" || query.StartSequence != 0 || query.EndSequence != 0 ||
		query.AfterSequence != 0
	if (query.Label == "") == !ranged || query.Label != "" && query.Limit != 0 ||
		query.Limit < 0 || query.Limit > 25 {
		return ProductionNumberPage{}, store.ErrInvalidBatesSelector
	}
	if query.Label != "" {
		match, err := v.metadata.FindPublishedProductionNumber(ctx, query.Label)
		if err != nil {
			return ProductionNumberPage{}, err
		}
		return ProductionNumberPage{Items: []ProductionNumberReference{productionNumberReference(match)}}, nil
	}
	limit := query.Limit
	if limit == 0 {
		limit = 25
	}
	page, err := v.metadata.FindPublishedProductionNumberRange(ctx, query.NamespaceID,
		query.StartSequence, query.EndSequence, query.AfterSequence, limit)
	if err != nil {
		return ProductionNumberPage{}, err
	}
	out := ProductionNumberPage{Items: make([]ProductionNumberReference, len(page.Items)),
		NextSequence: page.NextSequence}
	for i, item := range page.Items {
		out.Items[i] = productionNumberReference(item)
	}
	return out, nil
}

// ProductionNumberCandidates ranks exact, prefix and substring matches while
// preserving explicit ambiguity and truncation for a bounded public result.
func (v *Vault) ProductionNumberCandidates(ctx context.Context, query string, limit int) (ProductionNumberCandidates, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionNumberCandidates{}, ErrClosed
	}
	if limit == 0 {
		limit = 25
	}
	page, err := v.metadata.FindPublishedProductionNumberCandidates(ctx, query, limit)
	if err != nil {
		return ProductionNumberCandidates{}, err
	}
	out := ProductionNumberCandidates{MatchKind: page.MatchKind, Ambiguous: page.Ambiguous,
		Truncated: page.Truncated, Items: make([]ProductionNumberReference, len(page.Items))}
	for i, item := range page.Items {
		out.Items[i] = productionNumberReference(item)
	}
	return out, nil
}

func productionNumberReference(value store.PublishedProductionNumber) ProductionNumberReference {
	return ProductionNumberReference{
		Label: value.Label, JobID: value.JobID, SetID: value.SetID, Revision: value.Revision,
		ProductionReceiptSHA256: value.ProductionReceiptSHA256,
		ArtifactManifestSHA256:  value.ArtifactManifestSHA256,
		SourceVersionID:         value.SourceVersionID, OccurrenceID: value.OccurrenceID, Page: value.Page,
		ArtifactID: value.ArtifactID, ArtifactSHA256: value.ArtifactSHA256,
		ArtifactPath: value.ArtifactPath, Volume: value.Volume,
	}
}
