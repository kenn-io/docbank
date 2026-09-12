package processing

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type EmailPDFRenderer interface {
	Render(ctx context.Context, input emailpdf.HTML) ([]byte, int64, error)
}

// EmailPDFRuntime is an adapter for the existing durable rendition worker.
// It reads only retained version/generation authority captured by the job.
type EmailPDFRuntime struct {
	Catalog  *store.Store
	Blobs    *blob.Store
	Renderer EmailPDFRenderer
	Recipe   document.EmailPDFRecipeV1
	Spool    string
}

func (r *EmailPDFRuntime) Submit(ctx context.Context, input document.EmailPDFRequest) (document.EmailPDFJob, error) {
	if !input.Consent {
		return document.EmailPDFJob{}, store.ErrProcessingConsentRequired
	}
	if input.GenerationID == "" {
		v, err := r.Catalog.ContentVersionByID(ctx, input.VersionID)
		if err != nil {
			return document.EmailPDFJob{}, err
		}
		view, err := EnsureEmailTarget(ctx, r.Catalog, r.Blobs, r.Spool, store.EmailTarget{Version: v})
		if err != nil {
			return document.EmailPDFJob{}, err
		}
		input.GenerationID = view.Generation.ID
	}
	request, err := r.Request(ctx, input.VersionID, input.GenerationID, input.Paper)
	if err != nil {
		return document.EmailPDFJob{}, err
	}
	a := request.Authorization
	if _, err = r.Catalog.GrantConsent(ctx, store.ProcessingConsentGrantRequest{Principal: a.Principal, Scope: a.Scope, ProfileFingerprint: a.ProfileFingerprint, DisclosureFingerprint: a.DisclosureFingerprint, InputClasses: a.InputClasses, RetainedArtifactClasses: a.RetainedArtifactClasses}); err != nil {
		return document.EmailPDFJob{}, err
	}
	job, _, err := r.Catalog.EnqueueRenditionJob(ctx, request)
	if err != nil {
		return document.EmailPDFJob{}, err
	}
	out := document.EmailPDFJob{JobID: job.ID, VersionID: input.VersionID, ProfileFingerprint: request.Profile.Fingerprint, State: string(job.State)}
	if job.State == store.RenditionJobCompleted {
		receipt, err := r.Catalog.EmailPDFReceipt(ctx, input.VersionID, request.Profile.Fingerprint)
		if err != nil {
			return document.EmailPDFJob{}, err
		}
		out.Receipt = &receipt
	}
	return out, nil
}

func pdfProfileRecord(p document.ProcessingProfileV1) (store.ProcessingProfileRecord, error) {
	b, fp, err := document.CanonicalProfile(p)
	if err != nil {
		return store.ProcessingProfileRecord{}, err
	}
	return store.ProcessingProfileRecord{Fingerprint: fp.Profile, CanonicalProfile: b, RenditionRequestFingerprint: fp.RenditionRequest, EvidenceLexicalFingerprint: fp.EvidenceLexical, RetentionDisclosureFingerprint: fp.RetentionDisclosure, AttachmentPolicyFingerprint: p.RetentionDisclosure.AttachmentPolicyFingerprint, ConsentFingerprint: p.RetentionDisclosure.ConsentFingerprint, RenditionDisclosureFingerprint: p.Rendition.DisclosureFingerprint, TrustBoundary: p.RetentionDisclosure.TrustBoundary}, nil
}

func pdfPolicies() (document.EvidencePolicy, document.RenditionPolicy, error) {
	e, err := document.NewEvidencePolicy(64 << 20)
	if err != nil {
		return e, document.RenditionPolicy{}, err
	}
	r, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 64 << 20, MaxUnitRunes: 4_000_000, MaxSegmentRunes: 4096})
	return e, r, err
}

func pdfAuthorization(d document.RenditionDescriptor, p store.ProcessingProfileRecord, m document.AuthorizedUploadMetadata, at time.Time) document.RenditionAuthorization {
	return document.RenditionAuthorization{ProviderID: d.ID, DescriptorFingerprint: d.Fingerprint, PolicyFingerprint: d.PolicyFingerprint, RenditionRequestFingerprint: p.RenditionRequestFingerprint, SourceSHA256: m.SHA256, SourceBytes: m.ByteLength, CapabilityRecordChecksum: m.CapabilityRecordChecksum, ProviderMetadataChecksum: m.ProviderMetadataChecksum, MediaFamily: m.MediaFamily, MediaType: m.MediaType, InputKind: m.InputKind, AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactPDF}, MaxArtifacts: 1, MaxArtifactBytes: 256 << 20, MaxTotalResultBytes: 256 << 20, AuthorizedAt: at.UTC().Format(emailObservationTimeLayout), ExpiresAt: at.Add(5 * time.Minute).UTC().Format(emailObservationTimeLayout)}
}

func (r *EmailPDFRuntime) Request(ctx context.Context, versionID, generationID, paper string) (store.RenditionJobRequest, error) {
	v, err := r.Catalog.EmailMetadataGeneration(ctx, versionID, generationID)
	if err != nil {
		return store.RenditionJobRequest{}, err
	}
	if v.Evidence.Inventory == nil || v.Evidence.Inventory.State != document.EmailInventoryComplete {
		return store.RenditionJobRequest{}, store.ErrEmailPartUnavailable
	}
	recipe := r.Recipe
	if paper != "" {
		recipe.Paper = paper
	}
	if recipe.Paper == "" {
		recipe.Paper = "A4"
	}
	p, err := document.EmailPDFProfile(document.EmailPDFBindingV1{GenerationID: v.Generation.ID, GenerationChecksum: v.Generation.Checksum, Recipe: recipe})
	if err != nil {
		return store.RenditionJobRequest{}, err
	}
	profile, err := pdfProfileRecord(p)
	if err != nil {
		return store.RenditionJobRequest{}, err
	}
	d, err := document.EmailPDFDescriptor(recipe)
	if err != nil {
		return store.RenditionJobRequest{}, err
	}
	m := document.AuthorizedUploadMetadata{Filename: "message.eml", MediaFamily: "mail", MediaType: "message/rfc822", InputKind: document.RenditionInputOriginalFile, ByteLength: v.Version.Size, SHA256: v.Version.BlobHash, CapabilityRecordChecksum: v.Generation.ID, ProviderMetadataChecksum: v.Generation.Checksum}
	e, rp, err := pdfPolicies()
	if err != nil {
		return store.RenditionJobRequest{}, err
	}
	identity, err := document.NewRenditionExecutionIdentityV1(m, pdfAuthorization(d, profile, m, time.Now()), e, rp)
	if err != nil {
		return store.RenditionJobRequest{}, err
	}
	return store.RenditionJobRequest{ContentVersionID: versionID, Profile: profile, ExecutionIdentity: identity, CapturedArtifactPolicy: jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"email_pdf"},{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`), Authorization: store.ProviderOperationAuthorizationRequest{Principal: "local:email-pdf", Scope: "email-pdf:" + versionID, ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: profile.RenditionDisclosureFingerprint, InputClasses: []string{"original_file"}, RetainedArtifactClasses: []string{"email_pdf", "normalized_evidence", "sanitized_markdown"}}}, nil
}

func (r *EmailPDFRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	var p document.ProcessingProfileV1
	if err := json.Unmarshal(work.Profile.CanonicalProfile, &p); err != nil {
		return RenditionExecution{}, err
	}
	if p.Rendition == nil || p.Rendition.EmailPDF == nil || r.Renderer == nil {
		return RenditionExecution{}, ErrRenditionRuntimeUnavailable
	}
	b := p.Rendition.EmailPDF
	if b.Recipe.WorkerSHA256 != r.Recipe.WorkerSHA256 {
		return RenditionExecution{}, ErrRenditionRuntimeUnavailable
	}
	if b.Recipe.RendererSHA256 != r.Recipe.RendererSHA256 || b.Recipe.FontsSHA256 != r.Recipe.FontsSHA256 || b.Recipe.RendererVersion != r.Recipe.RendererVersion {
		return RenditionExecution{}, ErrRenditionRuntimeUnavailable
	}
	v, err := r.Catalog.EmailMetadataGeneration(ctx, work.Waiter.ContentVersionID, b.GenerationID)
	if err != nil {
		return RenditionExecution{}, err
	}
	if v.Generation.Checksum != b.GenerationChecksum || v.Version.BlobHash != work.Job.SourceSHA256 || v.Version.Size != work.ExecutionIdentity.Upload.ByteLength {
		return RenditionExecution{}, store.ErrEmailCorrupt
	}
	d, err := document.EmailPDFDescriptor(b.Recipe)
	if err != nil {
		return RenditionExecution{}, err
	}
	e, rp, err := pdfPolicies()
	if err != nil {
		return RenditionExecution{}, err
	}
	reader, size, err := r.Blobs.OpenStreamContext(ctx, v.Version.BlobHash)
	if err != nil {
		return RenditionExecution{}, err
	}
	if size != v.Version.Size {
		return RenditionExecution{}, errors.Join(store.ErrEmailCorrupt, reader.Close())
	}
	provider := &emailPDFProvider{runtime: r, view: v, binding: *b, descriptor: d}
	return RenditionExecution{Provider: provider, Upload: &emailPDFUpload{ReadCloser: reader, metadata: work.ExecutionIdentity.Upload}, Authorization: pdfAuthorization(d, work.Profile, work.ExecutionIdentity.Upload, now), EvidencePolicy: e, RenditionPolicy: rp}, nil
}

type emailPDFUpload struct {
	io.ReadCloser

	metadata document.AuthorizedUploadMetadata
}

func (u *emailPDFUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }

type emailPDFProvider struct {
	runtime    *EmailPDFRuntime
	view       store.EmailMetadataView
	binding    document.EmailPDFBindingV1
	descriptor document.RenditionDescriptor
}

func (p *emailPDFProvider) Descriptor() document.RenditionDescriptor { return p.descriptor }
func (p *emailPDFProvider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	started := time.Now().UTC()
	fail := func(err error) (document.RenditionResult, error) {
		e, _ := document.NewRenditionProviderError(document.RenditionErrorUnsupportedInput, 0, err)
		return document.RenditionResult{}, e
	}
	if _, err := io.Copy(io.Discard, upload); err != nil {
		return fail(err)
	}
	var selected []byte
	h, err := emailpdf.BuildHTML(ctx, p.view.Evidence, p.binding.Recipe.Paper, func(ctx context.Context, path string, ref document.EmailArtifactRefV1) (io.ReadCloser, error) {
		r, size, err := p.runtime.Blobs.OpenStreamContext(ctx, ref.SHA256)
		if err != nil {
			return nil, err
		}
		if size != ref.Size {
			return nil, errors.Join(store.ErrEmailCorrupt, r.Close())
		}
		return r, nil
	})
	if err != nil {
		return fail(err)
	}
	pdf, pages, err := p.runtime.Renderer.Render(ctx, h)
	if err != nil {
		return fail(err)
	}
	verifiedPages, err := emailpdf.VerifyPDF(pdf)
	if err != nil || pages != verifiedPages {
		return fail(errors.Join(err, errors.New("email PDF renderer page receipt disagrees with independent parser")))
	}
	r, _, err := p.runtime.Blobs.OpenStreamContext(ctx, h.BodySHA256)
	if err != nil {
		return fail(err)
	}
	selected, err = io.ReadAll(io.LimitReader(r, h.BodySize+1))
	err = errors.Join(err, r.Close())
	if err != nil {
		return fail(err)
	}
	kind := document.EmailBodyPlain
	for _, m := range p.view.Evidence.Inventory.Messages {
		for _, a := range m.Alternatives {
			if a.PartPath == h.BodyPath {
				kind = a.Kind
			}
		}
	}
	text, err := emailBodyText(ctx, kind, selected)
	if err != nil {
		return fail(err)
	}
	units, err := bodyUnits(ctx, text)
	if err != nil {
		return fail(err)
	}
	af, err := authorization.Fingerprint()
	if err != nil {
		return fail(err)
	}
	hash := renditionBytesSHA256(pdf)
	return document.RenditionResult{
		Evidence: document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance,
			Family:          "mail", UnitKind: document.EvidenceUnitGeneric, Units: units,
			Artifacts: []document.SourceEvidenceArtifactV1{{Pointer: "email/message.pdf", ProviderID: "email-pdf-output", Role: document.EvidenceArtifactPDF, SHA256: hash}},
			Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Selected body sidecar uses derived generic text blocks; exact MIME and complete PDF authority are retained separately."}},
		},
		Artifacts: []document.RenditionArtifact{{Role: document.EvidenceArtifactPDF, MediaType: "application/pdf", Payload: pdf, SHA256: hash}},
		Receipt: document.RenditionReceipt{
			ProviderID: p.descriptor.ID, DescriptorFingerprint: p.descriptor.Fingerprint,
			PolicyFingerprint:           p.descriptor.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    af, SourceSHA256: p.view.Version.BlobHash,
			OperationID: "email-pdf-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt:   started.Format(emailObservationTimeLayout),
			CompletedAt: time.Now().UTC().Format(emailObservationTimeLayout),
			EmailPDF:    &document.EmailPDFOutputV1{BodyPath: h.BodyPath, BodySHA256: h.BodySHA256, BodySize: h.BodySize, PDFSHA256: hash, PDFSize: int64(len(pdf)), Pages: pages},
			Usage:       document.RenditionUsage{Requests: 1, InputBytes: p.view.Version.Size, OutputBytes: int64(len(pdf)), Units: int64(len(units))},
		},
	}, nil
}
