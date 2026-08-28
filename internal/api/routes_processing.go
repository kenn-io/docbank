package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func registerProcessingRoutes(api huma.API, d Deps) {
	type profilesOutput struct{ Body []ProcessingProfileSummary }
	huma.Register(api, huma.Operation{
		OperationID: "listDocumentProcessingProfiles", Method: http.MethodGet,
		Path: "/api/v1/processing/profiles", Summary: "List locally executable document-processing profiles",
	}, func(_ context.Context, _ *struct{}) (*profilesOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		profiles := d.Processing.Profiles()
		result := make([]ProcessingProfileSummary, len(profiles))
		for index, profile := range profiles {
			result[index] = ProcessingProfileSummary{Name: profile.Name, Fingerprint: profile.Fingerprint,
				Rendition: profile.Rendition, EmbeddingBindings: profile.EmbeddingBindings}
		}
		return &profilesOutput{Body: result}, nil
	})

	type planInput struct{ Body ProcessingPlanRequest }
	type planOutput struct{ Body ProcessingPlan }
	huma.Register(api, huma.Operation{
		OperationID: "planDocumentProcessing", Method: http.MethodPost,
		Path: "/api/v1/processing/plans", Summary: "Preview provider disclosure for one document version",
	}, func(ctx context.Context, input *planInput) (*planOutput, error) {
		if d.Processing == nil {
			return nil, NewError(http.StatusServiceUnavailable, "processing_unavailable",
				"document processing is not configured")
		}
		plan, err := d.Processing.Plan(ctx, processing.Selector{
			NodeID: input.Body.Selector.NodeID, ContentVersionID: input.Body.Selector.ContentVersionID,
			Profile: input.Body.Selector.Profile,
		})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &planOutput{Body: fromProcessingPlan(plan)}, nil
	})

	type startInput struct{ Body StartProcessingRequest }
	processingEventSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[ProcessingJobEvent](), true, "ProcessingJobEvent")
	huma.Register(api, huma.Operation{
		OperationID: "startDocumentProcessing", Method: http.MethodPost,
		Path: "/api/v1/processing/jobs", Summary: "Start the exact reviewed document-processing plan",
		Description: "Returns exactly one job event followed by one terminal status event as newline-delimited JSON.",
		Responses: map[string]*huma.Response{"200": {Description: "Durable job identity and terminal status",
			Content: map[string]*huma.MediaType{"application/x-ndjson": {Schema: processingEventSchema}}}},
	}, func(ctx context.Context, input *startInput) (*huma.StreamResponse, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		job, err := d.Processing.Start(ctx, processing.StartRequest{
			Selector: processing.Selector{NodeID: input.Body.Selector.NodeID,
				ContentVersionID: input.Body.Selector.ContentVersionID, Profile: input.Body.Selector.Profile},
			PlanFingerprint: input.Body.PlanFingerprint, Consent: input.Body.Consent,
		})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		wireJob := ProcessingJob{ID: job.ID, RenditionJobID: job.RenditionJobID,
			AttachmentID: job.AttachmentID, EmbeddingJobIDs: job.EmbeddingJobIDs,
			ProfileFingerprint: job.ProfileFingerprint, ContentVersionID: job.ContentVersionID}
		status, err := d.Processing.Status(ctx, job.ID)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		wireStatus := fromProcessingStatus(status)
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Cache-Control", "no-store")
			stream := newEventStreamWriter[ProcessingJobEvent](hctx.BodyWriter(), func() {})
			stream.send(ProcessingJobEvent{Sequence: 1, Type: "job", Job: &wireJob})
			stream.send(ProcessingJobEvent{Sequence: 2, Type: "status", Status: &wireStatus, Terminal: true})
		}}, nil
	})

	type grantConsentInput struct{ Body ProcessingConsentGrantRequest }
	type grantConsentOutput struct{ Body ProcessingConsentGrant }
	huma.Register(api, huma.Operation{
		OperationID: "grantDocumentProcessingConsent", Method: http.MethodPost,
		Path: "/api/v1/processing/consent/grants", Summary: "Grant consent for one exact reviewed processing plan",
	}, func(ctx context.Context, input *grantConsentInput) (*grantConsentOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		var expiresAt *time.Time
		if input.Body.ExpiresAt != "" {
			parsed, err := time.Parse(time.RFC3339Nano, input.Body.ExpiresAt)
			if err != nil {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", "processing consent expiry is invalid")
			}
			expiresAt = &parsed
		}
		grant, err := d.Processing.GrantConsent(ctx, processing.ConsentGrantRequest{
			Selector: processing.Selector{NodeID: input.Body.Selector.NodeID,
				ContentVersionID: input.Body.Selector.ContentVersionID, Profile: input.Body.Selector.Profile},
			PlanFingerprint: input.Body.PlanFingerprint, ExpiresAt: expiresAt})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		result := ProcessingConsentGrant{PlanFingerprint: grant.PlanFingerprint,
			ProfileFingerprint: grant.ProfileFingerprint}
		if grant.ExpiresAt != nil {
			result.ExpiresAt = grant.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		return &grantConsentOutput{Body: result}, nil
	})

	type revokeConsentInput struct {
		Body ProcessingConsentRevokeRequest
	}
	type revokeConsentOutput struct{ Body ProcessingConsentRevocation }
	huma.Register(api, huma.Operation{
		OperationID: "revokeDocumentProcessingConsent", Method: http.MethodPost,
		Path: "/api/v1/processing/consent/revocations", Summary: "Revoke current operator processing consent",
	}, func(ctx context.Context, _ *revokeConsentInput) (*revokeConsentOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		revocation, err := d.Processing.RevokeConsent(ctx)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &revokeConsentOutput{Body: ProcessingConsentRevocation{
			RevokedAt: revocation.RevokedAt.UTC().Format(time.RFC3339Nano)}}, nil
	})

	type purgePlanInput struct{ Body DerivativePurgePlanRequest }
	type purgePlanOutput struct{ Body DerivativePurgePlan }
	huma.Register(api, huma.Operation{
		OperationID: "planDerivativePurge", Method: http.MethodPost,
		Path: "/api/v1/derivatives/purge-plans", Summary: "Preview one exact live derivative purge",
	}, func(ctx context.Context, input *purgePlanInput) (*purgePlanOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		plan, err := d.Processing.PlanDerivativePurge(ctx, processing.DerivativePurgeRequest{
			ContentVersionIDs: input.Body.ContentVersionIDs, AttachmentIDs: input.Body.AttachmentIDs,
			BuildIDs: input.Body.BuildIDs, All: input.Body.All})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &purgePlanOutput{Body: DerivativePurgePlan{Fingerprint: plan.Fingerprint,
			VaultUID: plan.VaultUID, ContentVersionIDs: plan.Request.ContentVersionIDs,
			AttachmentIDs: plan.Request.AttachmentIDs, BuildIDs: plan.Request.BuildIDs,
			All: plan.Request.All, ImmutableBackupCopiesUntouched: plan.ImmutableBackupCopiesUntouched}}, nil
	})

	type purgeJobInput struct{ Body DerivativePurgeJobRequest }
	purgeEventSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[DerivativePurgeEvent](), true, "DerivativePurgeEvent")
	huma.Register(api, huma.Operation{
		OperationID: "runDerivativePurge", Method: http.MethodPost,
		Path: "/api/v1/derivatives/purge-jobs", Summary: "Run one exact reviewed live derivative purge",
		Description: "Returns exactly one terminal receipt as newline-delimited JSON.",
		Responses: map[string]*huma.Response{"200": {Description: "Terminal bounded purge receipt",
			Content: map[string]*huma.MediaType{"application/x-ndjson": {Schema: purgeEventSchema}}}},
	}, func(ctx context.Context, input *purgeJobInput) (*huma.StreamResponse, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		receipt, err := d.Processing.RunDerivativePurge(ctx, processing.DerivativePurgeJobRequest{
			ContentVersionIDs: input.Body.ContentVersionIDs, AttachmentIDs: input.Body.AttachmentIDs,
			BuildIDs: input.Body.BuildIDs, All: input.Body.All,
			PlanFingerprint: input.Body.PlanFingerprint})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		wireReceipt := DerivativePurgeReceipt{ID: receipt.ID,
			PlanFingerprint: receipt.PlanFingerprint, RemovedHeads: receipt.RemovedHeads,
			RemovedAttachments: receipt.RemovedAttachments, RemovedBuilds: receipt.RemovedBuilds,
			RemovedArtifacts: receipt.RemovedArtifacts, RemovedLexicalSegments: receipt.RemovedLexicalSegments,
			RemovedEmbeddingHeads: receipt.RemovedEmbeddingHeads, RemovedEmbeddingSets: receipt.RemovedEmbeddingSets,
			PhysicalDerivativeBlobsReclaimed: receipt.PhysicalDerivativeBlobsReclaimed,
			ReclaimedFiles:                   receipt.ReclaimedFiles,
			ImmutableBackupCopiesUntouched:   receipt.ImmutableBackupCopiesUntouched}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Cache-Control", "no-store")
			stream := newEventStreamWriter[DerivativePurgeEvent](hctx.BodyWriter(), func() {})
			stream.send(DerivativePurgeEvent{Sequence: 1, Type: "result", Receipt: &wireReceipt, Terminal: true})
		}}, nil
	})

	type statusOutput struct{ Body ProcessingStatus }
	huma.Register(api, huma.Operation{
		OperationID: "getDocumentProcessingJob", Method: http.MethodGet,
		Path: "/api/v1/processing/jobs/{id}", Summary: "Read aggregate document-processing status",
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" pattern:"^[0-9a-f]{64}$"`
	}) (*statusOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		status, err := d.Processing.Status(ctx, input.ID)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &statusOutput{Body: fromProcessingStatus(status)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getDocumentRendition", Method: http.MethodGet,
		Path: "/api/v1/renditions/{attachment_id}", Summary: "Stream one exact active sanitized-Markdown rendition",
	}, func(ctx context.Context, input *struct {
		AttachmentID string `path:"attachment_id" pattern:"^[0-9a-f]{64}$"`
		MaxBytes     int64  `query:"max_bytes" default:"67108864" minimum:"1" maximum:"67108864"`
		Range        string `header:"Range"`
	}) (*huma.StreamResponse, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		rendition, err := d.Processing.RenditionByAttachment(ctx, input.AttachmentID, input.MaxBytes)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		if input.Range != "" {
			return renditionRangeStream(rendition, input.Range)
		}
		return renditionStream(rendition), nil
	})

	type coverageOutput struct{ Body CoverageReport }
	huma.Register(api, huma.Operation{
		OperationID: "getDocumentProcessingCoverage", Method: http.MethodGet,
		Path: "/api/v1/coverage", Summary: "Report rendition and embedding coverage for exact document versions",
	}, func(ctx context.Context, input *struct {
		Profile           string   `query:"profile" required:"true" minLength:"1" maxLength:"128" pattern:"^[a-z][a-z0-9_-]*$"`
		VaultUID          string   `query:"vault_uid" required:"true" format:"uuid"`
		ContentVersionIDs []string `query:"content_version_id" required:"true" minItems:"1" maxItems:"4096"`
	}) (*coverageOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		report, err := d.Processing.Coverage(ctx, input.Profile, processing.SourceFence{
			VaultUID: input.VaultUID, ContentVersionIDs: input.ContentVersionIDs})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &coverageOutput{Body: fromProcessingCoverage(report)}, nil
	})

	type searchInput struct{ Body DocumentSearchRequest }
	type searchOutput struct{ Body DocumentSearchReport }
	huma.Register(api, huma.Operation{
		OperationID: "searchDocuments", Method: http.MethodPost,
		Path: "/api/v1/search", Summary: "Search exact source-fenced document versions",
	}, func(ctx context.Context, input *searchInput) (*searchOutput, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		report, err := d.Processing.Search(ctx, processing.SearchRequest{Query: input.Body.Query,
			Mode: input.Body.Mode, Limit: input.Body.Limit, Profile: input.Body.Profile,
			BindingID: input.Body.BindingID, Explain: input.Body.Explain,
			Fence: processing.SourceFence{VaultUID: input.Body.Fence.VaultUID,
				ContentVersionIDs: input.Body.Fence.ContentVersionIDs}})
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &searchOutput{Body: fromDocumentSearchReport(report, input.Body.Explain)}, nil
	})
}

func processingUnavailable() error {
	return NewError(http.StatusServiceUnavailable, "processing_unavailable", "document processing is not configured")
}

func renditionStream(rendition processing.Rendition) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(ctx huma.Context) {
		defer func() { _ = rendition.Reader.Close() }()
		ctx.SetHeader("Content-Type", "text/markdown; charset=utf-8")
		ctx.SetHeader("Accept-Ranges", "bytes")
		ctx.SetHeader(RenditionAttachmentHeader, rendition.AttachmentID)
		ctx.SetHeader(RenditionBuildHeader, rendition.BuildID)
		ctx.SetHeader(RenditionArtifactHeader, rendition.ArtifactID)
		ctx.SetHeader(ContentVersionHeader, rendition.ContentVersionID)
		ctx.SetHeader(BlobHashHeader, rendition.SHA256)
		ctx.SetHeader(BlobSizeHeader, strconv.FormatInt(rendition.Size, 10))
		ctx.SetHeader("Trailer", "Content-Digest")
		hash := sha256.New()
		if _, err := io.Copy(ctx.BodyWriter(), io.TeeReader(rendition.Reader, hash)); err == nil {
			ctx.SetHeader("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash.Sum(nil))+":")
		}
	}}
}

func renditionRangeStream(rendition processing.Rendition, requested string) (*huma.StreamResponse, error) {
	data, readErr := io.ReadAll(io.LimitReader(rendition.Reader, rendition.Size+1))
	verifyErr := rendition.Reader.Verify()
	closeErr := rendition.Reader.Close()
	if readErr != nil || verifyErr != nil || closeErr != nil || int64(len(data)) != rendition.Size {
		return nil, NewError(http.StatusInternalServerError, "processing_failed", "document processing failed")
	}
	start, end, err := parseRenditionRange(requested, rendition.Size)
	if err != nil {
		return nil, NewError(http.StatusRequestedRangeNotSatisfiable, "invalid_rendition_range",
			"rendition byte range is invalid or unsatisfiable")
	}
	selected := bytes.Clone(data[start:end])
	return &huma.StreamResponse{Body: func(ctx huma.Context) {
		ctx.SetHeader("Content-Type", "text/markdown; charset=utf-8")
		ctx.SetHeader("Accept-Ranges", "bytes")
		ctx.SetHeader("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, rendition.Size))
		ctx.SetHeader(RenditionAttachmentHeader, rendition.AttachmentID)
		ctx.SetHeader(RenditionBuildHeader, rendition.BuildID)
		ctx.SetHeader(RenditionArtifactHeader, rendition.ArtifactID)
		ctx.SetHeader(ContentVersionHeader, rendition.ContentVersionID)
		ctx.SetHeader(BlobHashHeader, rendition.SHA256)
		ctx.SetHeader(BlobSizeHeader, strconv.FormatInt(rendition.Size, 10))
		ctx.SetHeader("Trailer", "Content-Digest")
		ctx.SetStatus(http.StatusPartialContent)
		hash := sha256.Sum256(selected)
		_, _ = ctx.BodyWriter().Write(selected)
		ctx.SetHeader("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(hash[:])+":")
	}}, nil
}

func parseRenditionRange(value string, size int64) (int64, int64, error) {
	if size < 1 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, errors.New("invalid rendition range")
	}
	spec := strings.TrimPrefix(value, "bytes=")
	left, right, ok := strings.Cut(spec, "-")
	if !ok || (left == "" && right == "") {
		return 0, 0, errors.New("invalid rendition range")
	}
	if left == "" {
		suffix, err := strconv.ParseInt(right, 10, 64)
		if err != nil || suffix < 1 {
			return 0, 0, errors.New("invalid rendition range")
		}
		return max(int64(0), size-suffix), size, nil
	}
	start, err := strconv.ParseInt(left, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, errors.New("invalid rendition range")
	}
	end := size
	if right != "" {
		inclusive, err := strconv.ParseInt(right, 10, 64)
		if err != nil || inclusive < start {
			return 0, 0, errors.New("invalid rendition range")
		}
		if inclusive < size-1 {
			end = inclusive + 1
		}
	}
	return start, end, nil
}

func fromProcessingCoverage(report processing.Coverage) CoverageReport {
	result := CoverageReport{VaultUID: report.VaultUID, ProfileFingerprint: report.ProfileFingerprint,
		State: report.State, Renditions: fromProcessingCoverageClass(report.Renditions),
		Embeddings: make([]CoverageClass, len(report.Embeddings))}
	for index, item := range report.Embeddings {
		result.Embeddings[index] = fromProcessingCoverageClass(item)
	}
	return result
}

func fromProcessingStatus(status processing.Status) ProcessingStatus {
	return ProcessingStatus{JobID: status.JobID, State: status.State, Phase: status.Phase,
		FailureCode: status.FailureCode, EmbeddingJobIDs: status.EmbeddingJobIDs,
		CompletedBindings: status.CompletedBindings}
}

func fromProcessingCoverageClass(item processing.CoverageClass) CoverageClass {
	return CoverageClass{Name: item.Name, Required: item.Required, State: item.State,
		Complete: item.Complete, Unavailable: item.Unavailable, Stale: item.Stale,
		Ineligible: item.Ineligible, Total: item.Total}
}

func fromDocumentSearchReport(report processing.SearchReport, explain bool) DocumentSearchReport {
	result := DocumentSearchReport{RequestedMode: string(report.RequestedMode), ActualMode: string(report.ActualMode),
		Coverage: DocumentSearchCoverage{BindingRequired: report.Coverage.BindingRequired,
			ScopedDocuments: report.Coverage.ScopedDocuments, CompleteDocuments: report.Coverage.CompleteDocuments,
			State: string(report.Coverage.State)}, Truncated: report.Truncated,
		Results: make([]DocumentSearchResult, len(report.Results)), Degradations: make([]string, len(report.Degradations))}
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

func fromProcessingPlan(plan processing.Plan) ProcessingPlan {
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

func fromProcessingError(err error) error {
	for _, item := range []struct {
		target error
		status int
		code   string
		detail string
	}{
		{processing.ErrForeignVault, http.StatusUnprocessableEntity, "foreign_vault", "source fence belongs to another vault"},
		{processing.ErrProfileNotConfigured, http.StatusUnprocessableEntity, "processing_profile_unavailable", "processing profile is not configured"},
		{processing.ErrPlanChanged, http.StatusConflict, "processing_plan_changed", "processing plan changed after preview"},
		{processing.ErrPurgePlanChanged, http.StatusConflict, "derivative_purge_plan_changed", "derivative purge plan changed after preview"},
		{processing.ErrInvalidPurgeRequest, http.StatusUnprocessableEntity, "invalid_derivative_purge", "derivative purge request is invalid"},
		{processing.ErrInvalidConsentExpiry, http.StatusUnprocessableEntity, "invalid_processing_consent_expiry", "processing consent expiry is invalid"},
		{store.ErrProcessingConsentRequired, http.StatusPreconditionRequired, "processing_consent_required", "processing consent is required"},
		{store.ErrProcessingConsentExpired, http.StatusPreconditionFailed, "processing_consent_expired", "processing consent has expired"},
		{store.ErrProcessingConsentRevoked, http.StatusPreconditionFailed, "processing_consent_revoked", "processing consent has been revoked"},
		{processing.ErrConsentRequired, http.StatusPreconditionRequired, "processing_consent_required", "processing consent is required"},
	} {
		if errors.Is(err, item.target) {
			return NewError(item.status, item.code, item.detail)
		}
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrVersionNodeMismatch) {
		return FromStoreError(err)
	}
	return NewError(http.StatusInternalServerError, "processing_failed", "document processing failed")
}
