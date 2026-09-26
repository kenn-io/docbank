package processing

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

var ErrContextInvalid = errors.New("context pack request is invalid")

const maxContextArtifactBytes = int64((8 << 20) + (256 << 10))

// ContextPackRequest selects either a current exact passage or an on-demand
// lexical search within one frozen current/live source fence. It never invokes
// a provider or creates document identities as a side effect of reading.
type ContextPackRequest struct {
	Fence                 SourceFence            `json:"fence"`
	Query                 string                 `json:"query,omitempty"`
	Seed                  *document.PassageRefV1 `json:"seed,omitempty"`
	Profile               string                 `json:"profile,omitempty"`
	MaxBytes              int                    `json:"max_bytes,omitempty"`
	PerDocumentPassages   int                    `json:"per_document_passages,omitempty"`
	MaxDocuments          int                    `json:"max_documents,omitempty"`
	IncludeSectionContext bool                   `json:"include_section_context,omitempty"`
}

func (service *Service) ContextPack(ctx context.Context, request ContextPackRequest) (ContextPack, error) {
	if service == nil || service.catalog == nil || service.blobs == nil ||
		(request.Query == "") == (request.Seed == nil) {
		return ContextPack{}, ErrContextInvalid
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = DefaultContextPackBytes
	}
	if request.PerDocumentPassages == 0 {
		request.PerDocumentPassages = DefaultContextPassagesPerDoc
	}
	if request.MaxDocuments == 0 {
		request.MaxDocuments = DefaultContextPackDocuments
	}
	if request.MaxBytes < 1 || request.MaxBytes > MaxContextPackBytes ||
		request.PerDocumentPassages < 1 || request.PerDocumentPassages > 8 ||
		request.MaxDocuments < 1 || request.MaxDocuments > DefaultContextPackDocuments ||
		len(request.Query) > 8192 || request.Fence.VaultUID != service.catalog.VaultID() {
		return ContextPack{}, ErrContextInvalid
	}
	resolved, err := service.ResolveSourceFence(ctx, SourceFenceResolveRequest{
		ContentVersionIDs: request.Fence.ContentVersionIDs})
	if err != nil {
		return ContextPack{}, err
	}
	if request.Fence.VaultUID != resolved.Fence.VaultUID ||
		!slices.Equal(sortedContextIDs(request.Fence.ContentVersionIDs), resolved.Fence.ContentVersionIDs) {
		return ContextPack{}, ErrContextInvalid
	}
	var candidates []contextCandidate
	omitted := map[string]int{}
	searchTruncated := false
	seedSectionTruncated := false
	profileFingerprint := ""
	var renditionCoverage Coverage
	var observedGeneration store.LexicalGeneration
	if request.Seed != nil {
		if !slices.Contains(resolved.Fence.ContentVersionIDs, request.Seed.ContentVersionID) {
			return ContextPack{}, ErrContextInvalid
		}
		candidates, seedSectionTruncated, err = service.seedContextCandidates(ctx,
			*request.Seed, request.IncludeSectionContext)
	} else {
		if request.Profile == "" {
			return ContextPack{}, ErrContextInvalid
		}
		profile, ok := service.profiles[request.Profile]
		if !ok {
			return ContextPack{}, ErrProfileNotConfigured
		}
		if profile.portable.Rendition == nil {
			return ContextPack{}, ErrContextInvalid
		}
		profileFingerprint = profile.record.Fingerprint
		renditionCoverage, err = service.Coverage(ctx, request.Profile, resolved.Fence)
		if err != nil {
			return ContextPack{}, err
		}
		observedGeneration, err = service.catalog.ActiveLexicalGeneration(ctx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return ContextPack{}, err
		}
		var report retrieval.Report
		report, err = service.Search(ctx, SearchRequest{
			Query: request.Query, Mode: string(retrieval.ModeLexical), Profile: request.Profile,
			Limit: MaxSearchLimit, Fence: resolved.Fence})
		if err == nil {
			currentGeneration, generationErr := service.catalog.ActiveLexicalGeneration(ctx)
			if generationErr != nil && !errors.Is(generationErr, store.ErrNotFound) {
				return ContextPack{}, generationErr
			}
			if currentGeneration.ID != observedGeneration.ID ||
				currentGeneration.ManifestDigest != observedGeneration.ManifestDigest {
				return ContextPack{}, store.ErrLexicalGenerationStale
			}
			searchTruncated = report.Truncated
			candidates, omitted, err = service.searchContextCandidates(ctx, profileFingerprint, report,
				request.MaxDocuments, request.PerDocumentPassages)
		}
	}
	if err != nil {
		return ContextPack{}, err
	}
	if seedSectionTruncated {
		omitted["section_context_truncated"]++
	}
	// A scope can be revoked or a current version replaced during assembly.
	// Recheck its exact IDs immediately before returning retained bytes.
	if _, err := service.ResolveSourceFence(ctx, SourceFenceResolveRequest{
		ContentVersionIDs: resolved.Fence.ContentVersionIDs}); err != nil {
		return ContextPack{}, err
	}
	pack, err := assembleContextPack(resolved.FenceFingerprint, len(resolved.Fence.ContentVersionIDs),
		candidates, request.MaxBytes, request.PerDocumentPassages, request.MaxDocuments)
	if err != nil {
		return ContextPack{}, err
	}
	pack.ProfileFingerprint = profileFingerprint
	if profileFingerprint != "" {
		pack.Coverage.RenditionAvailableSources = renditionCoverage.Renditions.Complete
		pack.Coverage.RenditionMissingSources = max(0,
			renditionCoverage.Renditions.Total-renditionCoverage.Renditions.Complete)
		pack.IndexGenerationID = observedGeneration.ID
		pack.IndexManifestDigest = observedGeneration.ManifestDigest
		if pack.Coverage.RenditionMissingSources > 0 {
			pack.Omitted["scope_rendition_missing"] += pack.Coverage.RenditionMissingSources
		}
	}
	for reason, count := range omitted {
		pack.Omitted[reason] += count
	}
	pack.SearchTruncated = searchTruncated
	pack, err = finishContextPackBudget(pack, request.MaxBytes)
	if err != nil {
		return ContextPack{}, err
	}
	if profileFingerprint != "" {
		currentGeneration, generationErr := service.catalog.ActiveLexicalGeneration(ctx)
		if generationErr != nil && !errors.Is(generationErr, store.ErrNotFound) {
			return ContextPack{}, generationErr
		}
		if currentGeneration.ID != observedGeneration.ID ||
			currentGeneration.ManifestDigest != observedGeneration.ManifestDigest {
			return ContextPack{}, store.ErrLexicalGenerationStale
		}
	}
	if err := revalidateContextPack(ctx, service.catalog, service.principal, pack); err != nil {
		return ContextPack{}, err
	}
	return pack, nil
}

func revalidateContextPack(ctx context.Context, catalog passageAuthorityCatalog,
	principal string, pack ContextPack,
) error {
	for _, passage := range pack.Passages {
		authority, err := catalog.ResolvePassageAuthority(ctx, passage.Ref)
		if err != nil || !authority.Fresh {
			return ErrPassageUnavailable
		}
		if err := checkPassageInputVisibility(ctx, catalog, authority, principal); err != nil {
			return ErrPassageUnavailable
		}
	}
	return nil
}

func sortedContextIDs(ids []string) []string {
	copyIDs := slices.Clone(ids)
	slices.Sort(copyIDs)
	return copyIDs
}

func (service *Service) seedContextCandidates(ctx context.Context, ref document.PassageRefV1,
	includeSection bool,
) ([]contextCandidate, bool, error) {
	return seedContextCandidates(ctx, service.catalog, service.blobs, service.principal, ref, includeSection)
}

func seedContextCandidates(ctx context.Context, catalog passageAuthorityCatalog,
	blobs verifiedBlobReader, principal string, ref document.PassageRefV1, includeSection bool,
) ([]contextCandidate, bool, error) {
	frontmatter, body, authority, err := loadPassageRendition(ctx, catalog, blobs, ref, principal)
	if err != nil {
		return nil, false, err
	}
	if !authority.Fresh {
		return nil, false, ErrPassageUnavailable
	}
	if !includeSection {
		return []contextCandidate{{Ref: ref, Body: body, Path: authority.Path,
			Reasons: []string{"exact_seed"}}}, false, nil
	}
	outline, err := buildPassageOutline(body, frontmatter.Rendition.BuildID,
		frontmatter.Rendition.BodySHA256, frontmatter.Navigation.Entries, authority.Build.Units)
	if err != nil {
		return nil, false, err
	}
	section, ok := containingContextSection(outline.Sections, ref.ByteStart, ref.ByteEnd)
	if !ok {
		return nil, false, ErrSectionNotFound
	}
	start, end := boundedContextWindow(body, section.ByteStart, section.ByteEnd,
		ref.ByteStart, ref.ByteEnd, DefaultPassageReadBytes)
	selected, err := document.NewPassageRefV1(ref, body, start, end)
	if err != nil {
		return nil, false, ErrPassageCorrupt
	}
	return []contextCandidate{{Ref: selected, Body: body, Path: authority.Path,
			Reasons: []string{"exact_seed", "section_context"}}},
		start > section.ByteStart || end < section.ByteEnd, nil
}

func containingContextSection(sections []OutlineSection, start, end int) (OutlineSection, bool) {
	for _, section := range sections {
		if section.ByteStart <= start && end <= section.ByteEnd {
			if child, ok := containingContextSection(section.Children, start, end); ok {
				return child, true
			}
			return section, true
		}
	}
	return OutlineSection{}, false
}

func boundedContextWindow(body []byte, sectionStart, sectionEnd, seedStart, seedEnd, limit int) (int, int) {
	if seedEnd-seedStart >= limit {
		end := min(seedStart+limit, sectionEnd)
		for end > seedStart && end < len(body) && !isContextRuneBoundary(body, end) {
			end--
		}
		return seedStart, end
	}
	start := max(sectionStart, seedStart-(limit-(seedEnd-seedStart))/2)
	end := min(sectionEnd, start+limit)
	start = max(sectionStart, end-limit)
	for start < seedStart && start < len(body) && !isContextRuneBoundary(body, start) {
		start++
	}
	for end > seedEnd && end < len(body) && !isContextRuneBoundary(body, end) {
		end--
	}
	return start, end
}

func isContextRuneBoundary(body []byte, offset int) bool {
	return offset == 0 || offset == len(body) || body[offset]&0xc0 != 0x80
}

func (service *Service) searchContextCandidates(ctx context.Context, profileFingerprint string,
	report retrieval.Report, maxDocuments, perDocument int,
) ([]contextCandidate, map[string]int, error) {
	candidates := make([]contextCandidate, 0, len(report.Results))
	omitted := map[string]int{}
	maxScanned := min(len(report.Results), maxDocuments*2)
	for index, result := range report.Results {
		if index >= maxScanned {
			omitted["candidate_scan_limit"]++
			continue
		}
		view, err := service.catalog.ActiveRendition(ctx, result.Document.ContentVersionID, profileFingerprint)
		if errors.Is(err, store.ErrNotFound) {
			omitted["missing_rendition"]++
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		artifactSize := int64(0)
		for _, artifact := range view.Build.Artifacts {
			if artifact.Role == sanitizedMarkdownRole {
				artifactSize = artifact.Size
				break
			}
		}
		if artifactSize < 1 || artifactSize > maxContextArtifactBytes {
			omitted["source_too_large_or_missing"]++
			continue
		}
		identity, err := service.catalog.DocumentIdentityByNode(ctx, result.Document.NodeID)
		if errors.Is(err, store.ErrDocumentIdentityUnavailable) {
			omitted["missing_identity"]++
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		reconstructed, spans, err := reconstructSegmentRanges(view.Build)
		if err != nil {
			return nil, nil, err
		}
		added := 0
		for _, evidence := range result.Evidence {
			if added >= perDocument {
				break
			}
			if evidence.Kind != "rendition_segment" || evidence.BuildID != view.Build.ID {
				continue
			}
			span, ok := spans[evidence.SegmentID]
			if !ok {
				continue
			}
			ref, err := document.NewPassageRefV1(document.PassageRefV1{
				VaultUID: service.catalog.VaultID(), DocumentUID: identity.DocumentUID,
				ContentVersionID: result.Document.ContentVersionID, SourceSHA256: view.Build.SourceSHA256,
				RenditionBuildID: view.Build.ID, AttachmentID: view.Attachment.ID,
			}, reconstructed, span.Start, span.End)
			if err != nil {
				return nil, nil, ErrPassageCorrupt
			}
			_, verifiedBody, authority, err := loadPassageRendition(ctx, service.catalog,
				service.blobs, ref, service.principal)
			if errors.Is(err, ErrPassageUnavailable) || errors.Is(err, ErrPassageUnauthorized) {
				omitted["source_unavailable"]++
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if !authority.Fresh || authority.Node.ID != result.Document.NodeID ||
				!bytes.Equal(verifiedBody, reconstructed) {
				omitted["source_changed"]++
				continue
			}
			candidates = append(candidates, contextCandidate{Ref: ref, Body: verifiedBody,
				Path: authority.Path, Reasons: []string{"lexical_segment"}})
			added++
		}
		if added == 0 {
			omitted["no_exact_passage_evidence"]++
		}
	}
	return candidates, omitted, nil
}
