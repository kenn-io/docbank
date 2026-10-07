package processing

import (
	"context"
	"errors"
	"fmt"

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
	for _, source := range request.MediaSources {
		if source.SourceID == "" || source.SourceVersionID == "" || len(source.SourceID) > 256 ||
			len(source.SourceVersionID) > 256 || !fence[source.ContentVersionID] {
			return retrieval.Report{}, ErrMediaSearchInvalid
		}
		if _, exists := selections[source]; exists {
			return retrieval.Report{}, ErrMediaSearchInvalid
		}
		selected, err := service.selectMediaTranscript(ctx, MediaTranscriptRequest{
			SourceID: source.SourceID, SourceVersionID: source.SourceVersionID, ContentVersionID: source.ContentVersionID}, true)
		if err != nil {
			return retrieval.Report{}, fmt.Errorf("select media source %s version %s: %w", source.SourceID, source.SourceVersionID, err)
		}
		selections[source] = selected
		if _, exists := completeVersions[source.ContentVersionID]; !exists {
			completeVersions[source.ContentVersionID] = true
		}
		if selected.result.EvidenceState != mediaTranscriptEvidenceReady {
			completeVersions[source.ContentVersionID] = false
			continue
		}
		key := store.SearchSelectedBuild{ContentVersionID: source.ContentVersionID, BuildID: selected.view.Build.ID}
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
	// Re-read every selected authority before returning evidence or coverage.
	for source, previous := range selections {
		current, err := service.selectMediaTranscript(ctx, MediaTranscriptRequest{
			SourceID: source.SourceID, SourceVersionID: source.SourceVersionID, ContentVersionID: source.ContentVersionID}, true)
		if err != nil {
			return retrieval.Report{}, err
		}
		if current.result.EvidenceState != previous.result.EvidenceState || current.view.Head != previous.view.Head ||
			current.inputID != previous.inputID {
			return retrieval.Report{}, ErrMediaSearchUnavailable
		}
	}
	for i := range report.Results {
		item := &report.Results[i]
		for j := range item.Evidence {
			evidence := &item.Evidence[j]
			key := store.SearchSelectedBuild{ContentVersionID: item.Document.ContentVersionID, BuildID: evidence.BuildID}
			sources := associations[key]
			if len(sources) == 0 {
				return retrieval.Report{}, fmt.Errorf("media search evidence escaped selection: %w", ErrMediaSearchUnavailable)
			}
			selected := selections[sources[0]]
			evidence.MediaSources, evidence.SuppliedInputID = sources, selected.inputID
			evidence.Origin, evidence.Completeness = selected.origin, string(selected.view.Build.Completeness)
		}
	}
	report.MediaSourceSelection, report.Coverage = true, coverage
	return report, nil
}
