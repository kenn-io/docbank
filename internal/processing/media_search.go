package processing

import (
	"context"
	"errors"
	"slices"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
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
	profile := service.profiles[request.Profile]
	scope := store.SearchOptions{SelectedBuilds: plan.builds}
	for _, build := range plan.builds {
		scope.ContentVersionIDs = append(scope.ContentVersionIDs, build.ContentVersionID)
	}
	slices.Sort(scope.ContentVersionIDs)
	scope.ContentVersionIDs = slices.Compact(scope.ContentVersionIDs)
	if provider := profile.embedders[prepared.bindingID]; provider != nil {
		descriptor := provider.Descriptor()
		scope.EmbeddingDescriptor, scope.VectorSpaceID = &descriptor, profile.vectorSpaces[prepared.bindingID]
	}
	nonlexical := prepared.requestedMode == retrieval.ModeSemantic || prepared.requestedMode == retrieval.ModeHybrid
	bindingRequired := false
	for _, binding := range profile.portable.Embeddings {
		if binding.Name == prepared.bindingID {
			bindingRequired = binding.Activation == document.EmbeddingRequired
		}
	}
	mode := prepared.mode
	if nonlexical && len(plan.builds) == 0 {
		mode = retrieval.ModeUnavailable
	}
	if len(plan.builds) != 0 && (mode == retrieval.ModeSemantic || mode == retrieval.ModeHybrid) {
		available, err := service.catalog.SelectedSemanticRowsAvailable(ctx, profile.record.Fingerprint, prepared.bindingID, scope)
		if err != nil {
			return retrieval.Report{}, err
		}
		if !available {
			if mode == retrieval.ModeHybrid {
				mode = retrieval.ModeLexical
			} else {
				mode = retrieval.ModeUnavailable
			}
		}
	}
	actual := mode
	if actual == retrieval.ModeAuto {
		actual = retrieval.ModeLexical
	}
	report := retrieval.Report{RequestedMode: prepared.requestedMode, ActualMode: actual, Results: []retrieval.Result{}}
	if len(plan.builds) != 0 && mode != retrieval.ModeUnavailable {
		execution := prepared
		execution.mode = mode
		fence, err := service.beginSearchEgress(ctx, profile, execution)
		if err != nil {
			return retrieval.Report{}, err
		}
		if fence != nil {
			defer fence.Close()
		}
		query := retrieval.Query{Text: request.Query, Mode: mode, Limit: max(profile.portable.Retrieval.LexicalLimit, profile.portable.Retrieval.VectorLimit),
			LexicalLimit: profile.portable.Retrieval.LexicalLimit, VectorLimit: profile.portable.Retrieval.VectorLimit, Scope: scope, ContentFirst: true,
			ProcessingProfileFingerprint: profile.record.Fingerprint, BindingID: prepared.bindingID, Authorization: prepared.authorization}
		report, err = prepared.searcher.Search(ctx, query)
		if err == retrieval.ErrSelectedEmbeddingsUnavailable {
			// Eligibility can disappear after the planning read. Never encode an
			// empty leased allowlist; hybrid can still serve exact lexical evidence.
			if mode == retrieval.ModeHybrid {
				query.Mode = retrieval.ModeLexical
				report, err = prepared.searcher.Search(ctx, query)
			} else {
				report = retrieval.Report{ActualMode: retrieval.ModeUnavailable, Results: []retrieval.Result{}}
				err = nil
			}
		}
		if err != nil {
			return retrieval.Report{}, err
		}
	}
	// Selection is reread even when no vector candidate was eligible.
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
	report.RequestedMode = prepared.requestedMode
	if prepared.requestedMode == retrieval.ModeHybrid && report.ActualMode == retrieval.ModeLexical {
		report.Degradations = append(report.Degradations, retrieval.DegradationSemanticUnavailable)
	}
	report.MediaSelections = plan.mediaSelections(request.MediaSources)
	coverage := plan.coverage()
	if nonlexical {
		coverage.BindingRequired = bindingRequired
		coverage.CompleteDocuments = 0
		for id, complete := range plan.complete {
			if complete && slices.Contains(report.Coverage.CompleteContentVersionIDs, id) {
				coverage.CompleteDocuments++
			}
		}
		if report.Coverage.State != retrieval.CoverageComplete || coverage.CompleteDocuments != coverage.ScopedDocuments {
			coverage.State = retrieval.CoverageIncomplete
		}
	}
	report.MediaSourceSelection, report.Coverage = true, coverage
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
			Origin: selected.origin, SuppliedInputID: selected.inputID, BuildID: selected.view.Build.ID,
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
