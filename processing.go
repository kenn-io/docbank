package docbank

import (
	"context"
	"errors"

	internalprocessing "go.kenn.io/docbank/internal/processing"
)

var (
	ErrForeignVault                 = internalprocessing.ErrForeignVault
	ErrProcessingProfileUnavailable = internalprocessing.ErrProfileNotConfigured
	ErrProcessingPlanChanged        = internalprocessing.ErrPlanChanged
	ErrProcessingConsentRequired    = internalprocessing.ErrConsentRequired
)

func (v *Vault) PlanProcessing(ctx context.Context, request ProcessingPlanRequest) (ProcessingPlan, error) {
	if err := v.begin(); err != nil {
		return ProcessingPlan{}, err
	}
	defer v.lifecycle.RUnlock()
	plan, err := v.processing.Plan(ctx, toProcessingSelector(request.Selector))
	if err != nil {
		return ProcessingPlan{}, err
	}
	return fromProcessingPlan(plan), nil
}

func (v *Vault) StartProcessing(ctx context.Context, request StartProcessingRequest) (ProcessingJob, error) {
	if err := v.begin(); err != nil {
		return ProcessingJob{}, err
	}
	defer v.lifecycle.RUnlock()
	job, err := v.processing.Start(ctx, internalprocessing.StartRequest{
		Selector:        toProcessingSelector(request.PlanRequest.Selector),
		PlanFingerprint: request.PlanFingerprint, Consent: request.Consent})
	if err != nil {
		return ProcessingJob{}, err
	}
	return ProcessingJob{ID: job.ID, RenditionJobID: job.RenditionJobID,
		EmbeddingJobIDs: job.EmbeddingJobIDs, ProfileFingerprint: job.ProfileFingerprint,
		ContentVersionID: job.ContentVersionID}, nil
}

func (v *Vault) ProcessingStatus(ctx context.Context, request ProcessingStatusRequest) (ProcessingStatus, error) {
	if err := v.begin(); err != nil {
		return ProcessingStatus{}, err
	}
	defer v.lifecycle.RUnlock()
	status, err := v.processing.Status(ctx, request.JobID)
	if err != nil {
		return ProcessingStatus{}, err
	}
	return ProcessingStatus{JobID: status.JobID, State: status.State, Phase: status.Phase,
		FailureCode: status.FailureCode, EmbeddingJobIDs: status.EmbeddingJobIDs,
		CompletedBindings: status.CompletedBindings}, nil
}

func (v *Vault) Rendition(ctx context.Context, request RenditionRequest) (*RenditionContent, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	rendition, err := v.processing.Rendition(ctx, toProcessingSelector(request.Selector), request.MaxBytes)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, err
	}
	if rendition.Reader == nil {
		v.lifecycle.RUnlock()
		return nil, errors.New("processing service returned no rendition reader")
	}
	return &RenditionContent{VaultUID: rendition.VaultUID, NodeID: rendition.NodeID,
		ContentVersionID: rendition.ContentVersionID, ProfileFingerprint: rendition.ProfileFingerprint,
		AttachmentID: rendition.AttachmentID, BuildID: rendition.BuildID, ArtifactID: rendition.ArtifactID,
		SHA256: rendition.SHA256, Size: rendition.Size, Completeness: rendition.Completeness,
		Warnings: rendition.Warnings,
		Reader:   &leasedReader{VerifiedReadCloser: rendition.Reader, release: v.lifecycle.RUnlock}}, nil
}

func (v *Vault) DocumentCoverage(ctx context.Context, request CoverageRequest) (CoverageReport, error) {
	if err := v.begin(); err != nil {
		return CoverageReport{}, err
	}
	defer v.lifecycle.RUnlock()
	report, err := v.processing.Coverage(ctx, request.Profile, internalprocessing.SourceFence{
		VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs})
	if err != nil {
		return CoverageReport{}, err
	}
	result := CoverageReport{VaultUID: report.VaultUID, ProfileFingerprint: report.ProfileFingerprint,
		State: report.State, Renditions: fromCoverageClass(report.Renditions),
		Embeddings: make([]CoverageClass, len(report.Embeddings))}
	for index, item := range report.Embeddings {
		result.Embeddings[index] = fromCoverageClass(item)
	}
	return result, nil
}

func (v *Vault) SearchDocuments(ctx context.Context, request DocumentSearchRequest) (DocumentSearchReport, error) {
	if err := v.begin(); err != nil {
		return DocumentSearchReport{}, err
	}
	defer v.lifecycle.RUnlock()
	report, err := v.processing.Search(ctx, internalprocessing.SearchRequest{Query: request.Query,
		Mode: string(request.Mode), Limit: request.Limit, Profile: request.Profile,
		BindingID: request.BindingID, Explain: request.Explain,
		Fence: internalprocessing.SourceFence{VaultUID: request.Fence.VaultUID,
			ContentVersionIDs: request.Fence.ContentVersionIDs}})
	if err != nil {
		return DocumentSearchReport{}, err
	}
	return fromSearchReport(report, request.Explain), nil
}

func toProcessingSelector(selector ProcessingSelector) internalprocessing.Selector {
	return internalprocessing.Selector{NodeID: selector.NodeID,
		ContentVersionID: selector.ContentVersionID, Profile: selector.Profile}
}

func fromProcessingPlan(plan internalprocessing.Plan) ProcessingPlan {
	result := ProcessingPlan{Fingerprint: plan.Fingerprint, VaultUID: plan.VaultUID,
		Selector: ProcessingSelector{NodeID: plan.Selector.NodeID,
			ContentVersionID: plan.Selector.ContentVersionID, Profile: plan.Selector.Profile},
		ProfileFingerprint: plan.ProfileFingerprint, DisclosedClasses: plan.DisclosedClasses,
		RetainedClasses: plan.RetainedClasses, ConsentRequired: plan.ConsentRequired,
		BackupConsequence: plan.BackupConsequence,
		Estimate: ProcessingEstimate{SourceBytes: plan.Estimate.SourceBytes,
			ProviderCalls: plan.Estimate.ProviderCalls, VectorSpaces: plan.Estimate.VectorSpaces},
		Flow: make([]ProcessingFlowHop, len(plan.Flow))}
	for index, hop := range plan.Flow {
		result.Flow[index] = ProcessingFlowHop{Capability: hop.Capability,
			ProviderID: hop.ProviderID, TrustBoundary: hop.TrustBoundary,
			InputClasses: hop.InputClasses}
	}
	return result
}

func fromCoverageClass(item internalprocessing.CoverageClass) CoverageClass {
	return CoverageClass{Name: item.Name, Required: item.Required, State: item.State,
		Complete: item.Complete, Unavailable: item.Unavailable, Stale: item.Stale,
		Ineligible: item.Ineligible, Total: item.Total}
}

func fromSearchReport(report internalprocessing.SearchReport, explain bool) DocumentSearchReport {
	result := DocumentSearchReport{RequestedMode: DocumentSearchMode(report.RequestedMode),
		ActualMode: DocumentSearchMode(report.ActualMode),
		Coverage: DocumentSearchCoverage{BindingRequired: report.Coverage.BindingRequired,
			ScopedDocuments:   report.Coverage.ScopedDocuments,
			CompleteDocuments: report.Coverage.CompleteDocuments, State: string(report.Coverage.State)},
		Truncated: report.Truncated, Results: make([]DocumentSearchResult, len(report.Results)),
		Degradations: make([]string, len(report.Degradations))}
	for index, degradation := range report.Degradations {
		result.Degradations[index] = string(degradation)
	}
	for index, item := range report.Results {
		converted := DocumentSearchResult{VaultUID: item.Document.VaultID, NodeID: item.Document.NodeID,
			ContentVersionID: item.Document.ContentVersionID, Rank: item.Rank, Score: item.Score,
			Path: item.Path, Excerpt: item.Excerpt, LexicalRank: item.LexicalRank,
			SemanticRank: item.SemanticRank, Evidence: make([]DocumentEvidenceReference, len(item.Evidence))}
		for evidenceIndex, evidence := range item.Evidence {
			converted.Evidence[evidenceIndex] = DocumentEvidenceReference{Kind: evidence.Kind,
				BuildID: evidence.BuildID, SegmentID: evidence.SegmentID,
				VectorSpaceID: evidence.VectorSpaceID, EmbeddingSetID: evidence.EmbeddingSetID,
				InputGenerationID: evidence.InputGenerationID, InputID: evidence.InputID,
				InputKind: string(evidence.InputKind), SourceManifestChecksum: evidence.SourceManifestChecksum}
		}
		result.Results[index] = converted
	}
	if explain {
		result.Trace = make([]DocumentSearchTrace, len(report.Trace))
		for index, event := range report.Trace {
			result.Trace[index] = DocumentSearchTrace{Code: string(event.Code), Count: event.Count}
		}
	}
	return result
}
