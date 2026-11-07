package processing

import (
	"cmp"
	"context"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

// applyValidity drops candidates that left the search scope while reranking
// waited, as retrieval does, and orders the rest with out-of-force matches last.
func (service *Service) applyValidity(ctx context.Context, report *retrieval.Report, scope store.SearchOptions, asOf string) error {
	results := report.Results
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.Document.NodeID
	}
	declarations, err := service.catalog.SearchResultValidity(ctx, scope, ids)
	if err != nil {
		return err
	}
	kept := results[:0]
	for _, r := range results {
		if _, ok := declarations[r.Document.NodeID]; ok {
			kept = append(kept, r)
		}
	}
	if len(kept) != len(results) {
		report.Truncated = true
	}
	results = kept
	for i := range results {
		r := &results[i]
		n := declarations[r.Document.NodeID]
		if !n.Declared {
			continue
		}
		starts := make([]document.SuccessorStart, len(n.Successors))
		for j, next := range n.Successors {
			starts[j] = document.SuccessorStart{From: next.EffectiveFrom}
		}
		state, _ := document.EvaluateValidity(asOf, n.Window, starts)
		v := &retrieval.ResultValidity{State: state, EffectiveFrom: n.Window.From, EffectiveTo: n.Window.To, SupersededBy: []retrieval.ValiditySuccessor{}}
		for _, next := range n.Successors {
			if document.SuccessorApplies(asOf, n.Window, document.SuccessorStart{From: next.EffectiveFrom}) {
				v.SupersededBy = append(v.SupersededBy, retrieval.ValiditySuccessor{NodeID: next.NodeID, Path: next.Path, EffectiveFrom: next.EffectiveFrom})
			}
		}
		r.Validity = v
	}
	outdated := func(r retrieval.Result) int {
		if r.Validity != nil && r.Validity.State != document.ValidityCurrent {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(results, func(a, b retrieval.Result) int { return cmp.Compare(outdated(a), outdated(b)) })
	for i := range results {
		results[i].Rank = i + 1
	}
	report.Results = results
	return nil
}
