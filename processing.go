package docbank

import (
	"context"
	"errors"

	"go.kenn.io/docbank/document"
	internalprocessing "go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
)

var (
	ErrEvidenceUnavailable          = internalprocessing.ErrEvidenceUnavailable
	ErrInvalidEvidenceRequest       = internalprocessing.ErrInvalidEvidenceRequest
	ErrInvalidRenditionWindow       = internalprocessing.ErrInvalidRenditionWindow
	ErrInvalidRenditionEncoding     = internalprocessing.ErrInvalidRenditionEncoding
	ErrForeignVault                 = internalprocessing.ErrForeignVault
	ErrProcessingProfileUnavailable = internalprocessing.ErrProfileNotConfigured
	ErrProcessingPlanChanged        = internalprocessing.ErrPlanChanged
	ErrProcessingConsentRequired    = internalprocessing.ErrConsentRequired
	ErrRenditionFailed              = internalprocessing.ErrRenditionFailed
	ErrRenditionOperatorRequired    = internalprocessing.ErrRenditionOperatorRequired
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

// StartProcessing drives rendition work, including provider retry delays, before
// running embeddings. Callers can bound the wait with a context deadline.
// Terminal rendition failures match ErrRenditionFailed; ambiguous outcomes match
// ErrRenditionOperatorRequired.
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
	return ProcessingJob{ID: job.ID, RenditionJobID: job.RenditionJobID, AttachmentID: job.AttachmentID,
		EmbeddingJobIDs: job.EmbeddingJobIDs, ProfileFingerprint: job.ProfileFingerprint,
		ContentVersionID: job.ContentVersionID}, nil
}

// GrantProcessingPlanConsent records explicit authority for one exact reviewed
// embedded processing plan without starting provider work.
func (v *Vault) GrantProcessingPlanConsent(
	ctx context.Context, request ProcessingConsentGrantRequest,
) (ProcessingConsentGrant, error) {
	if err := v.begin(); err != nil {
		return ProcessingConsentGrant{}, err
	}
	defer v.lifecycle.RUnlock()
	grant, err := v.processing.GrantConsent(ctx, internalprocessing.ConsentGrantRequest{
		Selector:        toProcessingSelector(request.PlanRequest.Selector),
		PlanFingerprint: request.PlanFingerprint, ExpiresAt: request.ExpiresAt,
	})
	return ProcessingConsentGrant{PlanFingerprint: grant.PlanFingerprint,
		ProfileFingerprint: grant.ProfileFingerprint, ExpiresAt: grant.ExpiresAt}, err
}

// RevokeProcessingPlanConsent advances the embedded operator's processing fence.
func (v *Vault) RevokeProcessingPlanConsent(ctx context.Context) (ProcessingConsentRevocation, error) {
	if err := v.begin(); err != nil {
		return ProcessingConsentRevocation{}, err
	}
	defer v.lifecycle.RUnlock()
	revocation, err := v.processing.RevokeConsent(ctx)
	return ProcessingConsentRevocation{RevokedAt: revocation.RevokedAt}, err
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

// ReadEvidenceWindow reads the exact cited rendition without processing work or
// fallback. Stale or hidden identities match ErrEvidenceUnavailable; malformed
// references match ErrInvalidEvidenceRequest, and offsets beyond EOF match
// ErrInvalidRenditionWindow. The vault lease covers the internal blob read and
// cleanup, and is released before returning the text window.
func (v *Vault) ReadEvidenceWindow(ctx context.Context, request EvidenceWindowRequest) (EvidenceWindow, error) {
	if err := v.begin(); err != nil {
		return EvidenceWindow{}, err
	}
	defer v.lifecycle.RUnlock()
	window, err := v.processing.ReadEvidenceWindow(ctx, internalprocessing.EvidenceWindowRequest{
		VaultUID: request.VaultUID, NodeID: request.NodeID, ContentVersionID: request.ContentVersionID, ContentSHA256: request.ContentSHA256,
		RenditionAttachmentID: request.RenditionAttachmentID, BuildID: request.BuildID, RenditionSHA256: request.RenditionSHA256,
		Offset: request.Offset, MaxChars: request.MaxChars,
	})
	if err != nil {
		return EvidenceWindow{}, err
	}
	return EvidenceWindow{VaultUID: window.VaultUID, NodeID: window.NodeID, ContentVersionID: window.ContentVersionID, ContentSHA256: window.ContentSHA256,
		RenditionAttachmentID: window.AttachmentID, BuildID: window.BuildID, RenditionSHA256: window.Checksum, Text: window.Text,
		ActualStart: window.ActualStart, ActualEnd: window.ActualEnd, NextOffset: window.NextOffset, EOF: window.EOF, ResponseBytes: window.ResponseBytes, MediaType: window.MediaType}, nil
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

// FormatCoverage reports the capability snapshot captured when the vault's
// configured processing providers were constructed.
func (v *Vault) FormatCoverage(_ context.Context) (document.FormatCoverageV1, error) {
	if err := v.begin(); err != nil {
		return document.FormatCoverageV1{}, err
	}
	defer v.lifecycle.RUnlock()
	return v.processing.FormatCoverage(), nil
}

// LookupFormat resolves a catalog id or extension against the vault's
// constructor-frozen format coverage snapshot.
func (v *Vault) LookupFormat(_ context.Context, query string) (document.FormatLookupV1, error) {
	if err := v.begin(); err != nil {
		return document.FormatLookupV1{}, err
	}
	defer v.lifecycle.RUnlock()
	return v.processing.LookupFormat(query), nil
}

func (v *Vault) SearchDocuments(ctx context.Context, request DocumentSearchRequest) (DocumentSearchReport, error) {
	if err := v.begin(); err != nil {
		return DocumentSearchReport{}, err
	}
	defer v.lifecycle.RUnlock()
	var sources []retrieval.MediaSourceSelector
	if request.MediaSources != nil {
		sources = make([]retrieval.MediaSourceSelector, len(request.MediaSources))
		for i, source := range request.MediaSources {
			sources[i] = retrieval.MediaSourceSelector(source)
		}
	}
	report, err := v.processing.Search(ctx, internalprocessing.SearchRequest{Query: request.Query,
		MediaSources: sources,
		ContentFirst: request.ContentFirst,
		Mode:         string(request.Mode), Limit: request.Limit, Profile: request.Profile,
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
			InputClasses: hop.InputClasses, DiscloseFilename: hop.DiscloseFilename, Filename: hop.Filename, RuntimeDisclosure: fromInternalRuntimeDisclosure(hop.RuntimeDisclosure)}
	}
	return result
}

func fromInternalRuntimeDisclosure(value internalprocessing.RuntimeDisclosure) ProcessingRuntimeDisclosure {
	return ProcessingRuntimeDisclosure{ImmediateProcessor: value.ImmediateProcessor,
		UltimateProcessor: value.UltimateProcessor, Endpoint: value.Endpoint,
		Deployment: value.Deployment, Model: value.Model, ModelRevision: value.ModelRevision,
		VectorSpace: value.VectorSpace, MetadataClasses: value.MetadataClasses,
		RetainedArtifactRoles: value.RetainedArtifactRoles}
}

func toInternalRuntimeDisclosure(value ProcessingRuntimeDisclosure) internalprocessing.RuntimeDisclosure {
	return internalprocessing.RuntimeDisclosure{ImmediateProcessor: value.ImmediateProcessor,
		UltimateProcessor: value.UltimateProcessor, Endpoint: value.Endpoint,
		Deployment: value.Deployment, Model: value.Model, ModelRevision: value.ModelRevision,
		VectorSpace: value.VectorSpace, MetadataClasses: value.MetadataClasses,
		RetainedArtifactRoles: value.RetainedArtifactRoles}
}

func toInternalRuntimeDisclosures(values map[string]ProcessingRuntimeDisclosure) map[string]internalprocessing.RuntimeDisclosure {
	result := make(map[string]internalprocessing.RuntimeDisclosure, len(values))
	for name, value := range values {
		result[name] = toInternalRuntimeDisclosure(value)
	}
	return result
}

func fromCoverageClass(item internalprocessing.CoverageClass) CoverageClass {
	return CoverageClass{Name: item.Name, Required: item.Required, State: item.State,
		Complete: item.Complete, Unavailable: item.Unavailable, Stale: item.Stale,
		Ineligible: item.Ineligible, Rebuilding: item.Rebuilding,
		PreviousGenerationServing: item.PreviousServing, Total: item.Total}
}

func fromSearchReport(report internalprocessing.SearchReport, explain bool) DocumentSearchReport {
	result := DocumentSearchReport{RequestedMode: DocumentSearchMode(report.RequestedMode),
		ActualMode:           DocumentSearchMode(report.ActualMode),
		MediaSourceSelection: report.MediaSourceSelection,
		Coverage: DocumentSearchCoverage{BindingRequired: report.Coverage.BindingRequired,
			ScopedDocuments:   report.Coverage.ScopedDocuments,
			CompleteDocuments: report.Coverage.CompleteDocuments, State: string(report.Coverage.State)},
		Truncated: report.Truncated, Results: make([]DocumentSearchResult, len(report.Results)),
		Degradations: make([]string, len(report.Degradations))}
	if report.MediaSelections != nil {
		result.MediaSelections = make([]DocumentMediaSelection, len(report.MediaSelections))
		for i, selection := range report.MediaSelections {
			result.MediaSelections[i] = DocumentMediaSelection(selection)
		}
	}
	for index, degradation := range report.Degradations {
		result.Degradations[index] = string(degradation)
	}
	for index, item := range report.Results {
		converted := DocumentSearchResult{VaultUID: item.Document.VaultID, NodeID: item.Document.NodeID,
			ContentVersionID: item.Document.ContentVersionID, Rank: item.Rank, Score: item.Score,
			Path: item.Path, Excerpt: item.Excerpt, LexicalRank: item.LexicalRank,
			SemanticRank: item.SemanticRank, Evidence: make([]DocumentEvidenceReference, len(item.Evidence))}
		for evidenceIndex, evidence := range item.Evidence {
			convertedEvidence := DocumentEvidenceReference{Kind: evidence.Kind,
				Origin: evidence.Origin, Completeness: evidence.Completeness, SuppliedInputID: evidence.SuppliedInputID,
				BuildID: evidence.BuildID, SegmentID: evidence.SegmentID,
				VectorSpaceID: evidence.VectorSpaceID, EmbeddingSetID: evidence.EmbeddingSetID,
				InputGenerationID: evidence.InputGenerationID, InputID: evidence.InputID,
				InputKind: string(evidence.InputKind), SourceManifestChecksum: evidence.SourceManifestChecksum}
			for _, source := range evidence.MediaSources {
				convertedEvidence.MediaSources = append(convertedEvidence.MediaSources, DocumentMediaSource(source))
			}
			if evidence.TimeSpan != nil {
				convertedEvidence.TimeSpan = &MediaTimeSpan{
					StartMS: evidence.TimeSpan.StartMS, EndMS: evidence.TimeSpan.EndMS,
				}
			}
			converted.Evidence[evidenceIndex] = convertedEvidence
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
