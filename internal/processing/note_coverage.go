package processing

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// PublishedNoteCoverage reports existing processing state for current accepted
// sources. It neither captures a bounded source fence nor starts processing.
func (service *Service) PublishedNoteCoverage(ctx context.Context, profileName string, selector store.PublishedNoteSelector) (Coverage, error) {
	profile, ok := service.profiles[profileName]
	if !ok {
		return Coverage{}, ErrProfileNotConfigured
	}
	bindings := make([]store.ProcessingCoverageBinding, len(profile.portable.Embeddings))
	for i, binding := range profile.portable.Embeddings {
		bindings[i] = store.ProcessingCoverageBinding{BindingID: binding.Name,
			VectorSpaceID: profile.vectorSpaces[binding.Name], Required: binding.Activation == document.EmbeddingRequired}
	}
	snapshot, err := service.catalog.ProcessingCoverage(ctx, store.ProcessingCoverageScope{
		PublishedNotes: &selector, ProcessingProfileFingerprint: profile.record.Fingerprint, Bindings: bindings,
	})
	if err != nil {
		return Coverage{}, err
	}
	return service.coverageReport(profile, snapshot), nil
}

// coverageReport keeps exact-fence and publication reports on one state engine.
func (service *Service) coverageReport(profile configuredProfile, snapshot store.ProcessingCoverageSnapshot) Coverage {
	toClass := func(item store.ProcessingClassCoverage) CoverageClass {
		return CoverageClass{Name: item.Name, Required: item.Required, State: item.State,
			Complete: item.Complete, Unavailable: item.Unavailable, Stale: item.Stale,
			Ineligible: item.Ineligible, Rebuilding: item.Rebuilding,
			PreviousServing: item.PreviousGenerationServing, Total: item.Total}
	}
	report := Coverage{VaultUID: service.catalog.VaultID(), ProfileFingerprint: profile.record.Fingerprint,
		State: "complete", Renditions: CoverageClass{Name: "rendition", State: "not_required", Total: snapshot.Renditions.Total}}
	// A description profile attaches no rendition, so there is none to cover.
	if profile.portable.Rendition != nil && profileRenditionInput(profile) == document.RenditionInputOriginalFile {
		report.Renditions = toClass(snapshot.Renditions)
		report.Renditions.Name, report.Renditions.Required = "rendition", true
		if report.Renditions.Complete+report.Renditions.Rebuilding != report.Renditions.Total {
			report.State = "partial"
		} else if report.Renditions.Rebuilding > 0 {
			report.State = "rebuilding"
		}
	}
	required, requiredIncomplete, requiredRebuilding := false, false, false
	optionalRebuilding, optionalIncomplete := false, false
	for _, coverage := range snapshot.Embeddings {
		item := toClass(coverage)
		report.Embeddings = append(report.Embeddings, item)
		if item.Required {
			required = true
			requiredIncomplete = requiredIncomplete || item.Complete+item.Rebuilding != item.Total
			requiredRebuilding = requiredRebuilding || item.Rebuilding > 0
		} else {
			optionalIncomplete = optionalIncomplete || item.Complete+item.Rebuilding != item.Total
			optionalRebuilding = optionalRebuilding || item.Rebuilding > 0
		}
	}
	if report.State == "partial" || requiredIncomplete || (!required && optionalIncomplete) {
		report.State = "partial"
	} else if requiredRebuilding || (!required && optionalRebuilding) {
		report.State = "rebuilding"
	}
	return report
}
