package processing

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

var ErrMediaSearchUnavailable = errors.New("selected media transcript is unavailable or changed")
var ErrMediaSearchInvalid = errors.New("media search selectors are invalid")

func (service *Service) searchMediaSources(ctx context.Context, request SearchRequest, ids []string, prepared preparedSearch) (retrieval.Report, error) {
	if len(request.MediaSources) > store.MaxSearchSourceFenceIDs ||
		(prepared.mode != retrieval.ModeLexical && prepared.mode != retrieval.ModeAuto) || request.Rerank {
		return retrieval.Report{}, ErrMediaSearchInvalid
	}
	fence := make(map[string]bool, len(ids))
	for _, id := range ids {
		fence[id] = true
	}
	selections := make(map[retrieval.MediaSource]mediaTranscriptSelection, len(request.MediaSources))
	associations := make(map[store.SearchSelectedBuild][]retrieval.MediaSource)
	options := store.SearchOptions{ContentVersionIDs: ids}
	coverage := retrieval.Coverage{State: retrieval.CoverageComplete}
	completeVersions := make(map[string]bool)
	requests := make([]MediaTranscriptRequest, 0, len(request.MediaSources))
	seen := make(map[retrieval.MediaSource]bool)
	for _, source := range request.MediaSources {
		parsed, parseErr := uuid.Parse(source.ContentVersionID)
		if source.SourceID == "" || source.SourceVersionID == "" || len(source.SourceID) > 256 ||
			len(source.SourceVersionID) > 256 || !fence[source.ContentVersionID] || parseErr != nil ||
			parsed[6]>>4 != 4 || parsed[8]>>6 != 2 || parsed.String() != source.ContentVersionID {
			return retrieval.Report{}, ErrMediaSearchInvalid
		}
		if seen[source] {
			return retrieval.Report{}, ErrMediaSearchInvalid
		}
		seen[source] = true
		requests = append(requests, MediaTranscriptRequest{source.SourceID, source.SourceVersionID, source.ContentVersionID})
	}
	selectedItems, err := service.selectMediaTranscripts(ctx, requests)
	if err != nil {
		return retrieval.Report{}, err
	}
	ready := make([]MediaTranscriptRequest, 0, len(requests))
	for i, source := range request.MediaSources {
		selected := selectedItems[requests[i]]
		selections[source] = selected
		if _, exists := completeVersions[source.ContentVersionID]; !exists {
			completeVersions[source.ContentVersionID] = true
		}
		if selected.result.EvidenceState != mediaTranscriptEvidenceReady {
			completeVersions[source.ContentVersionID] = false
			continue
		}
		key := store.SearchSelectedBuild{ContentVersionID: source.ContentVersionID, BuildID: selected.view.Build.ID}
		ready = append(ready, requests[i])
		if _, exists := associations[key]; !exists {
			options.SelectedBuilds = append(options.SelectedBuilds, key)
		}
		associations[key] = append(associations[key], source)
	}
	coverage.ScopedDocuments = len(completeVersions)
	for _, complete := range completeVersions {
		if complete {
			coverage.CompleteDocuments++
		}
	}
	if coverage.ScopedDocuments != coverage.CompleteDocuments {
		coverage.State = retrieval.CoverageIncomplete
	}
	report := retrieval.Report{RequestedMode: prepared.mode, ActualMode: retrieval.ModeLexical, Results: []retrieval.Result{}}
	if len(options.SelectedBuilds) != 0 {
		var err error
		report, err = prepared.searcher.Search(ctx, retrieval.Query{Text: request.Query, Mode: prepared.mode,
			Limit: prepared.limit, LexicalLimit: service.profiles[request.Profile].portable.Retrieval.LexicalLimit,
			Scope: options, ContentFirst: true})
		if err != nil {
			return retrieval.Report{}, err
		}
	}
	// Re-read authority for every source that could contribute evidence.
	currentItems, err := service.selectMediaTranscripts(ctx, ready)
	if err != nil {
		return retrieval.Report{}, err
	}
	for _, request := range ready {
		source := retrieval.MediaSource{SourceID: request.SourceID, SourceVersionID: request.SourceVersionID, ContentVersionID: request.ContentVersionID}
		previous, current := selections[source], currentItems[request]
		if current.result.EvidenceState != previous.result.EvidenceState || current.view.Head != previous.view.Head ||
			current.inputID != previous.inputID {
			if completeVersions[source.ContentVersionID] {
				coverage.CompleteDocuments--
				completeVersions[source.ContentVersionID] = false
			}
			coverage.State = retrieval.CoverageIncomplete
			delete(selections, source)
		}
	}
	kept := report.Results[:0]
	for i := range report.Results {
		item := &report.Results[i]
		if len(item.Evidence) != 1 {
			return retrieval.Report{}, fmt.Errorf("media search evidence cardinality: %w", ErrMediaSearchUnavailable)
		}
		for j := range item.Evidence {
			evidence := &item.Evidence[j]
			key := store.SearchSelectedBuild{ContentVersionID: item.Document.ContentVersionID, BuildID: evidence.BuildID}
			sources := associations[key]
			if len(sources) == 0 {
				return retrieval.Report{}, fmt.Errorf("media search evidence escaped selection: %w", ErrMediaSearchUnavailable)
			}
			live := make([]retrieval.MediaSource, 0, len(sources))
			for _, source := range sources {
				if _, ok := selections[source]; ok {
					live = append(live, source)
				}
			}
			if len(live) == 0 {
				item.Evidence = nil
				break
			}
			sources = live
			selected := selections[sources[0]]
			evidence.MediaSources, evidence.SuppliedInputID = sources, selected.inputID
			evidence.Origin, evidence.Completeness = selected.origin, string(selected.view.Build.Completeness)
		}
		if len(item.Evidence) != 0 {
			item.Rank = len(kept) + 1
			kept = append(kept, *item)
		}
	}
	report.Results = kept
	report.MediaSourceSelection, report.Coverage = true, coverage
	return report, nil
}
