package processing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pdftext"
	"go.kenn.io/docbank/internal/store"
)

const PDFTextProfileName = "docbank-pdf-text"
const PDFTextMaxDocumentBytes = 500 << 20

// NewPDFTextProfile constructs the daemon's local PDF text profile.
func NewPDFTextProfile() (string, ProfileConfig, error) {
	provider, err := pdftext.New(pdftext.Profile{MaxDocumentBytes: PDFTextMaxDocumentBytes, MaxPages: 5_000})
	if err != nil {
		return "", ProfileConfig{}, err
	}
	sourceBinding := store.LengthPrefixedSHA256("docbank/pdf-text-source/v1")
	descriptor := provider.Descriptor()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{ //nolint:gosec // Contains a stable non-secret local binding identity.
			Name: "docbank-pdf-text", AdapterContract: pdftext.AdapterContract,
			Descriptor:               document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			AuthorizationFingerprint: store.LengthPrefixedSHA256("docbank/pdf-text-authorization/v1", sourceBinding),
			CredentialBinding:        "credential:local-pdf-text",
			DeploymentFingerprint:    sourceBinding, DisclosureFingerprint: store.LengthPrefixedSHA256(
				"docbank/pdf-text-disclosure/v1", sourceBinding),
			TrustBoundary: string(descriptor.TrustBoundary), MaxDocumentBytes: PDFTextMaxDocumentBytes,
			MaxResponseBytes: 16 << 20, MaxUnits: 5_000,
			RequestedArtifacts:       []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			UploadOptionsFingerprint: store.LengthPrefixedSHA256("docbank/pdf-text-upload/v1", sourceBinding),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     store.LengthPrefixedSHA256("docbank/pdf-text-completeness/v1"),
			LexicalSegmenterFingerprint: store.LengthPrefixedSHA256("docbank/pdf-text-segmenter/v1"),
			MaxDocumentChars:            16 << 20, MaxSegmentRunes: 1 << 20, MaxUnitRunes: 1 << 20,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      store.LengthPrefixedSHA256("docbank/pdf-text-normalizer/v1"),
			RenditionContract:          document.RenditionContractV1,
			SanitizerFingerprint:       store.LengthPrefixedSHA256("docbank/pdf-text-sanitizer/v1"),
			SourceEvidenceContract:     document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: store.LengthPrefixedSHA256("docbank/pdf-text-attachments/v1"),
			ConsentFingerprint:          store.LengthPrefixedSHA256("docbank/pdf-text-consent/v1", sourceBinding),
			RetainSanitizedMarkdown:     true, RetainTypedArtifacts: true,
			TrustBoundary: string(descriptor.TrustBoundary),
		},
	}
	return PDFTextProfileName, ProfileConfig{Profile: profile, RenditionProvider: provider}, nil
}

// PDFHasNoTextLayer reads a retained build through the canonical evidence reader.
func (service *Service) PDFHasNoTextLayer(ctx context.Context, buildID string) (bool, error) {
	build, err := service.catalog.RenditionBuild(ctx, buildID)
	if err != nil {
		return false, err
	}
	evidence, _, err := service.renditionEvidence(ctx, build)
	if err != nil {
		return false, err
	}
	if evidence.Family != "pdf" || len(evidence.Units) != 0 || len(evidence.Omissions) == 0 {
		return false, nil
	}
	for _, omission := range evidence.Omissions {
		if omission.Reason != "no_text_layer" {
			return false, nil
		}
	}
	return true, nil
}

// PDFTextRunner admits automatic PDFs through the shared rendition lifecycle.
type PDFTextRunner struct {
	Service *Service
	mu      sync.Mutex
	skips   map[string]bool
}

// Initialize grants local consent only when no prior decision exists.
func (r *PDFTextRunner) Initialize(ctx context.Context) error {
	p := r.Service.profiles[PDFTextProfileName]
	for _, request := range r.Service.profileConsentRequests(p) {
		_, err := r.Service.catalog.AuthorizeProviderOperation(ctx, request)
		state, err := processingConsentState(err)
		if err != nil {
			return err
		}
		if state == "required" {
			return r.Service.grantProfileConsent(ctx, p, nil)
		}
	}
	return nil
}

func (r *PDFTextRunner) targets(ctx context.Context, after string, limit int) ([]store.DocumentSummary, error) {
	return r.listTargets(ctx, after, limit, false)
}

func (r *PDFTextRunner) listTargets(ctx context.Context, after string, limit int, includeWaiting bool) ([]store.DocumentSummary, error) {
	active, err := r.Service.ProfileConsentActive(ctx, PDFTextProfileName)
	if err != nil || !active {
		return nil, err
	}
	p := r.Service.profiles[PDFTextProfileName]
	var out []store.DocumentSummary
	for len(out) < limit {
		pageSize := limit - len(out)
		items, err := r.Service.catalog.PDFTextTargetsAfter(ctx, p.record.Fingerprint, after, p.portable.Rendition.MaxDocumentBytes, pageSize, includeWaiting)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		for _, item := range items {
			if !r.skips[item.ContentVersionID] {
				out = append(out, item)
			}
		}
		r.mu.Unlock()
		if len(items) < pageSize {
			return out, nil
		}
		after = items[len(items)-1].ContentVersionID
	}
	return out, nil
}

func (r *PDFTextRunner) process(ctx context.Context, item store.DocumentSummary) error {
	s := r.Service
	p := s.profiles[PDFTextProfileName]
	node, err := s.catalog.NodeByID(ctx, item.NodeID)
	if err == nil {
		var version store.ContentVersion
		version, err = s.catalog.ContentVersionByID(ctx, item.ContentVersionID)
		if err == nil {
			if node.CurrentVersionID != version.ID {
				return nil
			}
			_, _, err = s.enqueueRendition(ctx, node, version, p, s.renditionConsentRequest(p), "", false)
		}
	}
	if errors.Is(err, store.ErrRenditionJobStaleAuthority) {
		return nil
	}
	if errors.Is(err, ErrSourceIneligible) || errors.Is(err, ErrPDFPasswordRequired) || errors.Is(err, ErrPlanChanged) || errors.Is(err, ErrRenditionFailed) || errors.Is(err, ErrRenditionOperatorRequired) || errors.Is(err, ErrConsentRequired) || errors.Is(err, store.ErrProcessingConsentRevoked) || errors.Is(err, store.ErrProcessingConsentExpired) || errors.Is(err, store.ErrNotFound) {
		r.mu.Lock()
		if r.skips == nil {
			r.skips = make(map[string]bool)
		}
		r.skips[item.ContentVersionID] = true
		r.mu.Unlock()
		return nil
	}
	return err
}

// Pending includes current sources and jobs that have not published yet.
func (r *PDFTextRunner) Pending(ctx context.Context) (bool, error) {
	items, err := r.listTargets(ctx, "", 1, true)
	return len(items) > 0, err
}

func (r *PDFTextRunner) Backfill(ctx context.Context, logger *slog.Logger) error {
	lane := Backfill[store.DocumentSummary]{Name: "pdf-text", Page: 50, IdleDelay: time.Second, MaxIdleDelay: time.Second, Logger: logger, WriteRevision: r.Service.catalog.WriteRevision, List: r.targets, Key: func(v store.DocumentSummary) string { return v.ContentVersionID }, Process: r.process}
	return lane.Run(ctx)
}
