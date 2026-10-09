package processing

import (
	"context"
	"errors"
	"slices"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

var ErrMediaSearchInvalid = errors.New("media search selectors are invalid")

// MaxMediaSearchSuppliedInputIDs bounds one selector's supplied input set.
const MaxMediaSearchSuppliedInputIDs = 64

// mediaSearchPlan tracks the selected sources that may contribute evidence and
// whether every selector for each content version has ready evidence.
type mediaSearchPlan struct {
	selections   map[retrieval.MediaSource]mediaTranscriptSelection
	associations map[store.SearchSelectedBuild][]retrieval.MediaSource
	builds       []store.SearchSelectedBuild
	ready        []MediaTranscriptRequest
	complete     map[string]bool
}

func (service *Service) searchMediaSources(
	ctx context.Context, request SearchRequest, ids []string, prepared preparedSearch,
) (retrieval.Report, error) {
	requests, err := validateMediaSearch(request, ids)
	if err != nil {
		return retrieval.Report{}, err
	}
	selected, err := service.selectMediaTranscripts(ctx, requests)
	if err != nil {
		return retrieval.Report{}, err
	}
	plan := newMediaSearchPlan(request.MediaSources, requests, selected)
	report := retrieval.Report{RequestedMode: prepared.mode, ActualMode: retrieval.ModeLexical,
		Results: []retrieval.Result{}}
	if len(plan.builds) != 0 {
		candidateLimit := service.profiles[request.Profile].portable.Retrieval.LexicalLimit
		report, err = prepared.searcher.Search(ctx, retrieval.Query{Text: request.Query,
			Mode: prepared.mode, Limit: candidateLimit, LexicalLimit: candidateLimit,
			Scope: store.SearchOptions{SelectedBuilds: plan.builds}, ContentFirst: true})
		if err != nil {
			return retrieval.Report{}, err
		}
	}
	// Re-read authority for every source that could contribute evidence.
	current, err := service.selectMediaTranscripts(ctx, plan.ready)
	if err != nil {
		return retrieval.Report{}, err
	}
	plan.dropChanged(current)
	report.Results = plan.attributedResults(report.Results)
	if len(report.Results) > prepared.limit {
		report.Results = report.Results[:prepared.limit]
		report.Truncated = true
	}
	report.MediaSelections = plan.mediaSelections(request.MediaSources)
	report.MediaSourceSelection, report.Coverage = true, plan.coverage()
	return report, nil
}

func validateMediaSearch(
	request SearchRequest, ids []string,
) ([]MediaTranscriptRequest, error) {
	sources := request.MediaSources
	if len(sources) == 0 || len(sources) > store.MaxSearchSourceFenceIDs {
		return nil, ErrMediaSearchInvalid
	}
	fence := make(map[string]bool, len(ids))
	for _, id := range ids {
		fence[id] = true
	}
	seen := make(map[retrieval.MediaSource]bool, len(sources))
	requests := make([]MediaTranscriptRequest, 0, len(sources))
	for _, source := range sources {
		identity := source.Identity()
		if !validMediaSelectorID(source.SourceID) || !validMediaSelectorID(source.SourceVersionID) ||
			!fence[source.ContentVersionID] || seen[identity] ||
			len(source.SuppliedInputIDs) > MaxMediaSearchSuppliedInputIDs ||
			slices.ContainsFunc(source.SuppliedInputIDs, invalidSuppliedInputID) {
			return nil, ErrMediaSearchInvalid
		}
		seen[identity] = true
		requests = append(requests, MediaTranscriptRequest(identity))
	}
	return requests, nil
}

// validMediaSelectorID bounds source identities in UTF-8 bytes; Huma counts characters.
func validMediaSelectorID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id)
}

func invalidSuppliedInputID(id string) bool {
	return !canonical.IsSHA256Hex(id)
}

func newMediaSearchPlan(
	sources []retrieval.MediaSourceSelector, requests []MediaTranscriptRequest,
	selected map[MediaTranscriptRequest]mediaTranscriptSelection,
) mediaSearchPlan {
	plan := mediaSearchPlan{
		selections:   make(map[retrieval.MediaSource]mediaTranscriptSelection, len(sources)),
		associations: make(map[store.SearchSelectedBuild][]retrieval.MediaSource),
		ready:        make([]MediaTranscriptRequest, 0, len(requests)),
		complete:     make(map[string]bool),
	}
	for i, source := range sources {
		selection := selected[requests[i]]
		if _, exists := plan.complete[source.ContentVersionID]; !exists {
			plan.complete[source.ContentVersionID] = true
		}
		if !mediaSelectionEligible(source, selection) {
			plan.complete[source.ContentVersionID] = false
			continue
		}
		identity := source.Identity()
		key := store.SearchSelectedBuild{ContentVersionID: source.ContentVersionID,
			BuildID: selection.view.Build.ID}
		if _, exists := plan.associations[key]; !exists {
			plan.builds = append(plan.builds, key)
		}
		plan.associations[key] = append(plan.associations[key], identity)
		plan.selections[identity] = selection
		plan.ready = append(plan.ready, requests[i])
	}
	return plan
}

// mediaSelectionEligible admits ready evidence. Supplied input constraints
// exclude only supplied transcripts; generated transcripts stay eligible.
func mediaSelectionEligible(
	source retrieval.MediaSourceSelector, selection mediaTranscriptSelection,
) bool {
	if selection.result.EvidenceState != mediaTranscriptEvidenceReady {
		return false
	}
	return selection.origin != "supplied" || source.SuppliedInputIDs == nil ||
		slices.Contains(source.SuppliedInputIDs, selection.inputID)
}

// dropChanged removes sources whose authority changed while the search ran.
func (plan *mediaSearchPlan) dropChanged(
	current map[MediaTranscriptRequest]mediaTranscriptSelection,
) {
	for _, request := range plan.ready {
		source := retrieval.MediaSource(request)
		previous, now := plan.selections[source], current[request]
		if now.result.EvidenceState != previous.result.EvidenceState ||
			now.view.Head != previous.view.Head || now.inputID != previous.inputID {
			plan.complete[source.ContentVersionID] = false
			delete(plan.selections, source)
		}
	}
}

// attributedResults keeps results whose builds still have a stable selected
// source and renumbers their ranks.
func (plan *mediaSearchPlan) attributedResults(results []retrieval.Result) []retrieval.Result {
	kept := results[:0]
	for i := range results {
		item := results[i]
		if !plan.attributeEvidence(&item) {
			continue
		}
		item.Rank = len(kept) + 1
		kept = append(kept, item)
	}
	return kept
}

func (plan *mediaSearchPlan) attributeEvidence(item *retrieval.Result) bool {
	for i := range item.Evidence {
		evidence := &item.Evidence[i]
		key := store.SearchSelectedBuild{ContentVersionID: item.Document.ContentVersionID,
			BuildID: evidence.BuildID}
		var live []retrieval.MediaSource
		for _, source := range plan.associations[key] {
			if _, ok := plan.selections[source]; ok {
				live = append(live, source)
			}
		}
		if len(live) == 0 {
			return false
		}
		evidence.MediaSources = live
	}
	return len(item.Evidence) != 0
}

func (plan *mediaSearchPlan) mediaSelections(
	sources []retrieval.MediaSourceSelector,
) []retrieval.MediaSelection {
	result := make([]retrieval.MediaSelection, 0, len(plan.selections))
	for _, source := range sources {
		selected, ok := plan.selections[source.Identity()]
		if !ok {
			continue
		}
		result = append(result, retrieval.MediaSelection{SourceID: source.SourceID,
			SourceVersionID: source.SourceVersionID, ContentVersionID: source.ContentVersionID,
			Origin: selected.origin, SuppliedInputID: selected.inputID,
			Completeness: string(selected.view.Build.Completeness)})
	}
	return result
}

func (plan *mediaSearchPlan) coverage() retrieval.Coverage {
	coverage := retrieval.Coverage{State: retrieval.CoverageComplete,
		ScopedDocuments: len(plan.complete)}
	for _, complete := range plan.complete {
		if complete {
			coverage.CompleteDocuments++
		}
	}
	if coverage.CompleteDocuments != coverage.ScopedDocuments {
		coverage.State = retrieval.CoverageIncomplete
	}
	return coverage
}
