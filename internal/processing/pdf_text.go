package processing

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pdftext"
	"go.kenn.io/docbank/internal/store"
)

const PDFTextProfileName = "docbank-pdf-text"
const PDFTextMaxDocumentBytes = 500 << 20

var ErrInvalidCoverageCursor = errors.New("invalid or stale coverage cursor")

type coverageCursor struct {
	Version     int    `json:"version"`
	Scope       string `json:"scope"`
	Manifest    string `json:"manifest"`
	Index       int    `json:"index"`
	Offset      int    `json:"offset"`
	Unavailable bool   `json:"unavailable,omitempty"`
}

func readPDFCoveragePage(ctx context.Context, ids []string, builds map[string]string, scope, manifest, cursor string, read func(context.Context, string) ([]int, error)) ([]PageTextGap, string, bool, error) {
	pos := coverageCursor{Version: 1, Scope: scope, Manifest: manifest}
	if cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		pos = coverageCursor{}
		if err != nil || json.Unmarshal(data, &pos, json.RejectUnknownMembers(true)) != nil || pos.Version != 1 || pos.Scope != scope || pos.Manifest != manifest || pos.Index < 0 || pos.Index >= len(ids) || pos.Offset < 0 {
			return nil, "", false, ErrInvalidCoverageCursor
		}
	}
	type result struct {
		pages []int
		err   error
	}
	cache := make(map[string]result)
	var gaps []PageTextGap
	remaining := 8192
	for i := pos.Index; i < len(ids); i++ {
		build := builds[ids[i]]
		if build == "" {
			if i == pos.Index && pos.Offset != 0 {
				return nil, "", false, ErrInvalidCoverageCursor
			}
			continue
		}
		r, ok := cache[build]
		if !ok {
			r.pages, r.err = read(ctx, build)
			cache[build] = r
		}
		if ctx.Err() != nil {
			return nil, "", false, ctx.Err()
		}
		if r.err != nil {
			pos.Unavailable = true
			continue
		}
		start := 0
		if i == pos.Index {
			start = pos.Offset
		}
		if start > len(r.pages) {
			return nil, "", false, ErrInvalidCoverageCursor
		}
		end := min(len(r.pages), start+remaining)
		if end > start {
			gaps = append(gaps, PageTextGap{ContentVersionID: ids[i], Pages: r.pages[start:end]})
			remaining -= end - start
		}
		if remaining == 0 {
			if end < len(r.pages) {
				pos.Index, pos.Offset = i, end
			} else {
				pos.Index, pos.Offset = i+1, 0
			}
			if pos.Index < len(ids) {
				data, err := json.Marshal(pos)
				if err != nil {
					return nil, "", false, err
				}
				return gaps, base64.RawURLEncoding.EncodeToString(data), pos.Unavailable, nil
			}
			break
		}
	}
	return gaps, "", pos.Unavailable, nil
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
	found := false
	for _, artifact := range build.Artifacts {
		if artifact.Role == "normalized_evidence" {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("normalized PDF evidence is unavailable")
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
	slices.Sort(pages)
	pages = slices.Compact(pages)
	return pages, nil
}

// PDFSourcePagesWithoutText reads gaps from the built-in profile for a source version.
func (service *Service) PDFSourcePagesWithoutText(ctx context.Context, versionID string) ([]int, error) {
	profile, ok := service.profiles[PDFTextProfileName]
	if !ok {
		return nil, nil
	}
	buildID, err := service.catalog.PDFTextCandidate(ctx, versionID, profile.record.Fingerprint)
	if err != nil || buildID == "" {
		return nil, err
	}
	return service.PDFPagesWithoutText(ctx, buildID)
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

// Pending includes current sources and jobs that have not published yet.
func (r *PDFTextRunner) Pending(ctx context.Context) (bool, error) {
	items, err := r.listTargets(ctx, "", 1, true)
	return len(items) > 0, err
}

func (r *PDFTextRunner) Backfill(ctx context.Context, logger *slog.Logger) error {
	lane := Backfill[store.DocumentSummary]{Name: "pdf-text", Page: 50, IdleDelay: time.Second, MaxIdleDelay: time.Second, Logger: logger, WriteRevision: r.Service.catalog.WriteRevision, List: r.targets, Key: func(v store.DocumentSummary) string { return v.ContentVersionID }, Process: r.process}
	return lane.Run(ctx)
}
