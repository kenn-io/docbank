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

// readPDFCoveragePage bounds both evidence reads and returned page numbers.
func readPDFCoveragePage(ctx context.Context, ids []string, builds map[string]string, read func(context.Context, string) ([]int, error)) ([]PageTextGap, string, bool, error) {
	type result struct {
		pages []int
		err   error
	}
	cache := make(map[string]result)
	var gaps []PageTextGap
	remaining := 8192
	unavailable := false
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, "", false, err
		}
		build := builds[id]
		if build == "" {
			continue
		}
		r, ok := cache[build]
		if !ok {
			if len(cache) == 16 {
				return gaps, statusPartial, unavailable, nil
			}
			r.pages, r.err = read(ctx, build)
			cache[build] = r
		}
		if err := ctx.Err(); err != nil {
			return nil, "", false, err
		}
		if r.err != nil {
			unavailable = true
			continue
		}
		end := min(len(r.pages), remaining)
		if end > 0 {
			gaps = append(gaps, PageTextGap{ContentVersionID: id, Pages: r.pages[:end]})
			remaining -= end
		}
		if remaining == 0 {
			if end < len(r.pages) {
				return gaps, statusPartial, unavailable, nil
			}
			for _, later := range ids[i+1:] {
				if builds[later] != "" {
					return gaps, statusPartial, unavailable, nil
				}
			}
			return gaps, "", unavailable, nil
		}
	}
	return gaps, "", unavailable, nil
}

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

// PDFPagesWithoutText reads omissions from one retained build.
func (service *Service) PDFPagesWithoutText(ctx context.Context, buildID string) ([]int, error) {
	build, err := service.catalog.RenditionBuild(ctx, buildID)
	if err != nil {
		return nil, err
	}
	evidence, _, err := service.renditionEvidence(ctx, build)
	if err != nil {
		return nil, err
	}
	var pages []int
	if evidence.Family == "pdf" {
		for _, omission := range evidence.Omissions {
			if omission.Reason == "no_text_layer" && omission.Locator != nil {
				pages = append(pages, int(omission.Locator.Start))
			}
		}
	}
	return pages, nil
}

// PDFSourcePagesWithoutText reads gaps from the built-in profile for a source version.
func (service *Service) PDFSourcePagesWithoutText(ctx context.Context, versionID string) ([]int, error) {
	profile, ok := service.profiles[PDFTextProfileName]
	if !ok {
		return nil, nil
	}
	builds, err := service.catalog.PDFTextCandidates(ctx, []string{versionID}, profile.record.Fingerprint)
	buildID := builds[versionID]
	if err != nil || buildID == "" {
		return nil, err
	}
	return service.PDFPagesWithoutText(ctx, buildID)
}

// PDFPageGaps reads bounded page omissions for the exact source fence.
func (service *Service) PDFPageGaps(ctx context.Context, fence SourceFence) ([]PageTextGap, string, bool, error) {
	profile, ids, err := service.profileFence(PDFTextProfileName, fence)
	if err != nil {
		return nil, "", false, err
	}
	builds, err := service.catalog.PDFTextCandidates(ctx, ids, profile.record.Fingerprint)
	if err != nil {
		return nil, "", false, err
	}
	return readPDFCoveragePage(ctx, ids, builds, service.PDFPagesWithoutText)
}

// PDFTextRunner admits automatic PDFs through the shared rendition lifecycle.
type PDFTextRunner struct {
	Service    *Service
	mu         sync.Mutex
	skips      map[string]bool
	skipWarned bool
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
		const pageSize = 1000
		items, err := r.Service.catalog.PDFTextTargetsAfter(ctx, p.record.Fingerprint, after, p.portable.Rendition.MaxDocumentBytes, pageSize, includeWaiting)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		for _, item := range items {
			if !r.skips[item.ContentVersionID] {
				out = append(out, item)
				if len(out) == limit {
					break
				}
			}
		}
		r.mu.Unlock()
		if len(out) == limit {
			return out, nil
		}
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
		if node.TrashedAt != nil {
			return nil
		}
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
		current, readErr := s.catalog.NodeByID(ctx, item.NodeID)
		if errors.Is(readErr, store.ErrNotFound) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if current.TrashedAt != nil || current.CurrentVersionID != item.ContentVersionID || current.Revision != node.Revision {
			return nil
		}
	}
	if errors.Is(err, store.ErrRenditionJobStaleAuthority) || errors.Is(err, ErrSourceIneligible) || errors.Is(err, ErrPDFPasswordRequired) || errors.Is(err, ErrPlanChanged) || errors.Is(err, ErrRenditionFailed) || errors.Is(err, ErrRenditionOperatorRequired) || errors.Is(err, ErrConsentRequired) || errors.Is(err, store.ErrProcessingConsentRevoked) || errors.Is(err, store.ErrProcessingConsentExpired) || errors.Is(err, store.ErrNotFound) {
		r.mu.Lock()
		if r.skips == nil {
			r.skips = make(map[string]bool)
		}
		if len(r.skips) < localTextSkipLimit {
			r.skips[item.ContentVersionID] = true
		} else if !r.skipWarned {
			slog.Warn("PDF text skip limit reached; additional ineligible files will be inspected again", "limit", localTextSkipLimit)
			r.skipWarned = true
		}
		r.mu.Unlock()
		return nil
	}
	return err
}

// UnqueuedPending reports which eligible sources the runner has yet to queue.
func (r *PDFTextRunner) UnqueuedPending(ctx context.Context) (func(string, int64) bool, error) {
	active, err := r.Service.ProfileConsentActive(ctx, PDFTextProfileName)
	if err != nil || !active {
		return nil, err
	}
	maxBytes := r.Service.profiles[PDFTextProfileName].portable.Rendition.MaxDocumentBytes
	return func(version string, size int64) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return size > 0 && size <= maxBytes && !r.skips[version]
	}, nil
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
