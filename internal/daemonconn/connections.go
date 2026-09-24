package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/processing"
)

const connectionSemanticMethod = "semantic"
const maxConnectionSuggestionResponseBytes = 8 << 20

// SuggestConnections requests stored, exact-passage evidence from the selected
// daemon and checks the returned authority before exposing it to callers.
func (c *Connection) SuggestConnections(ctx context.Context, request api.ConnectionSuggestionRequest) (api.ConnectionSuggestionReport, error) {
	if err := validateConnectionRequest(request); err != nil {
		return api.ConnectionSuggestionReport{}, fmt.Errorf("connection request is invalid: %w", err)
	}
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).SuggestConnections(runtime.WithStreamingResponse(ctx),
		&apiclient.SuggestConnectionsRequestOptions{Body: &request})
	if err != nil {
		return api.ConnectionSuggestionReport{}, err
	}
	defer func() { _ = responseHTTP.Body.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(responseHTTP.Body, maxConnectionSuggestionResponseBytes+1))
	if err != nil {
		return api.ConnectionSuggestionReport{}, errors.New("reading connection response")
	}
	if len(encoded) > maxConnectionSuggestionResponseBytes {
		return api.ConnectionSuggestionReport{}, errors.New("connection response exceeds byte limit")
	}
	var report api.ConnectionSuggestionReport
	if err := json.Unmarshal(encoded, &report); err != nil {
		return api.ConnectionSuggestionReport{}, errors.New("connection response is invalid")
	}
	if err := validateConnectionReport(request, report); err != nil {
		return api.ConnectionSuggestionReport{}, fmt.Errorf("connection response is invalid: %w", err)
	}
	return report, nil
}

func validateConnectionRequest(request api.ConnectionSuggestionRequest) error {
	if err := document.ValidatePassageIdentityV1(request.Source); err != nil {
		return err
	}
	if request.Fence.VaultUID != request.Source.VaultUID ||
		len(request.Fence.ContentVersionIDs) == 0 || len(request.Fence.ContentVersionIDs) > processing.MaxSourceFenceIDs ||
		!slices.Contains(request.Fence.ContentVersionIDs, request.Source.ContentVersionID) ||
		!validBoundedSearchIdentity(request.Profile, 128) || !validBoundedSearchIdentity(request.BindingID, 128) ||
		request.Limit < 0 || request.Limit > 100 ||
		!slices.Contains([]string{"", "passage", "document"}, request.SeedKind) ||
		!slices.Contains([]string{"", connectionSemanticMethod, "lexical", "tag", "hybrid"}, request.Method) {
		return errors.New("source, fence, method, or bounds are inconsistent")
	}
	seen := make(map[string]bool, len(request.Fence.ContentVersionIDs))
	for _, id := range request.Fence.ContentVersionIDs {
		if !validUUIDv4(id) || seen[id] {
			return errors.New("source fence contains invalid or duplicate versions")
		}
		seen[id] = true
	}
	return nil
}

func validateConnectionReport(request api.ConnectionSuggestionRequest, report api.ConnectionSuggestionReport) error {
	method, seedKind, limit := request.Method, request.SeedKind, request.Limit
	if method == "" {
		method = connectionSemanticMethod
	}
	if seedKind == "" {
		seedKind = "passage"
	}
	if limit == 0 {
		limit = 10
	}
	if report.Source != request.Source || report.Method != method || report.SeedKind != seedKind ||
		report.Candidates == nil || report.CandidateCount != len(report.Candidates) ||
		report.CandidateCount > limit || report.FallbackMethods == nil || report.SeedSegments == nil ||
		report.SeedSegmentCount != len(report.SeedSegments) || report.SeedSegmentCount > 4096 {
		return errors.New("source identity or candidate bounds changed")
	}
	switch report.State {
	case "ready":
		if report.CoverageReason != "" || report.Aggregation == "" || report.ScoreMetric == "" {
			return errors.New("ready report has unavailable coverage")
		}
	case "unavailable":
		if report.CoverageReason == "" || report.CandidateCount != 0 || report.Truncated {
			return errors.New("unavailable report contains candidates or lacks a reason")
		}
	default:
		return errors.New("unknown report state")
	}
	if method == "tag" || method == "hybrid" {
		if report.State != "unavailable" {
			return errors.New("tag passage evidence is unavailable")
		}
	}
	if method == "lexical" && report.SeedSegmentCount != 0 {
		return errors.New("lexical report cites semantic seed segments")
	}
	if report.State == "ready" && method == connectionSemanticMethod && report.SeedSegmentCount == 0 &&
		(!report.Truncated || report.CandidateCount != 0) {
		return errors.New("semantic report lacks selected seed segments")
	}
	if report.State == "ready" && method == connectionSemanticMethod &&
		(!validSHA256Hex(report.VectorSpaceID) || !validSHA256Hex(report.IndexGenerationID) ||
			!validSHA256Hex(report.SourceManifestChecksum) || !validSHA256Hex(report.SourceEmbeddingSetID)) {
		return errors.New("semantic report lacks vector or index provenance")
	}
	seedSegments := make(map[string]api.ConnectionSeedSegment, len(report.SeedSegments))
	seedBytes := 0
	for _, segment := range report.SeedSegments {
		if !sameConnectionRendition(request.Source, segment.Passage) ||
			!validConnectionQuote(segment.Passage, segment.Quote) ||
			!validBoundedSearchIdentity(segment.InputID, 128) ||
			!validBoundedSearchIdentity(segment.GenerationID, 128) ||
			segment.Span.UnitIndex < 0 || segment.Span.CharStart < 0 || segment.Span.CharEnd <= segment.Span.CharStart ||
			seedSegments[segment.InputID].InputID != "" ||
			(seedKind == "passage" && (segment.Passage.ByteStart >= request.Source.ByteEnd ||
				segment.Passage.ByteEnd <= request.Source.ByteStart)) {
			return errors.New("selected seed segment provenance is inconsistent")
		}
		seedBytes += len(segment.Quote)
		if seedBytes > 1<<20 {
			return errors.New("selected seed evidence exceeds the response budget")
		}
		seedSegments[segment.InputID] = segment
	}
	seen := make(map[string]bool, len(report.Candidates))
	versions := map[string]bool{request.Source.ContentVersionID: true}
	for _, candidate := range report.Candidates {
		if candidate.Source != request.Source || candidate.Method != method ||
			!slices.Contains(request.Fence.ContentVersionIDs, candidate.Target.ContentVersionID) ||
			candidate.Target.VaultUID != request.Fence.VaultUID || versions[candidate.Target.ContentVersionID] ||
			candidate.TargetNodeID < 1 || !strings.HasPrefix(candidate.TargetPath, "/") ||
			candidate.ScoreMetric != report.ScoreMetric || candidate.Reason == "" || candidate.Aggregation != report.Aggregation ||
			math.IsNaN(candidate.Score) || math.IsInf(candidate.Score, 0) ||
			candidate.DuplicateMembers == nil || candidate.DuplicateCount != len(candidate.DuplicateMembers) ||
			!validSHA256Hex(candidate.ID) || seen[candidate.ID] ||
			!validConnectionQuote(candidate.Source, candidate.SourceQuote) ||
			!validConnectionQuote(candidate.Target, candidate.TargetQuote) {
			return errors.New("candidate evidence escapes the fence or is inconsistent")
		}
		if method == connectionSemanticMethod && (candidate.VectorSpaceID != report.VectorSpaceID ||
			candidate.IndexGenerationID != report.IndexGenerationID || candidate.InputID == "" ||
			candidate.SourceInputID == "" || candidate.SourceSegment == nil || candidate.IndexGenerationID == "" ||
			candidate.SourceSpan == nil || candidate.TargetSpan == nil ||
			!sameSeedSegment(candidate.SourceInputID, candidate.SourceSegment, candidate.SourceSegmentQuote,
				candidate.SourceSpan, candidate.SourceGeneration, seedSegments)) {
			return errors.New("semantic candidate has missing segment provenance")
		}
		seen[candidate.ID] = true
		versions[candidate.Target.ContentVersionID] = true
		for _, member := range candidate.DuplicateMembers {
			if !slices.Contains(request.Fence.ContentVersionIDs, member.Target.ContentVersionID) ||
				member.Target.VaultUID != request.Fence.VaultUID || versions[member.Target.ContentVersionID] ||
				member.Target.SourceSHA256 != candidate.Target.SourceSHA256 ||
				member.TargetNodeID < 1 || !strings.HasPrefix(member.TargetPath, "/") ||
				!validSHA256Hex(member.ID) || seen[member.ID] ||
				!validConnectionQuote(member.Target, member.TargetQuote) ||
				member.ScoreMetric != report.ScoreMetric || math.IsNaN(member.Score) || math.IsInf(member.Score, 0) ||
				member.Score > candidate.Score {
				return errors.New("duplicate member evidence escapes the fence or is inconsistent")
			}
			if method == connectionSemanticMethod && (member.SourceSegment == nil || member.SourceSpan == nil ||
				member.TargetSpan == nil || member.InputID == "" || member.VectorSpaceID != candidate.VectorSpaceID ||
				member.EmbeddingSetID == "" || member.InputGenerationID == "" ||
				member.IndexGenerationID != candidate.IndexGenerationID ||
				!sameSeedSegment(member.SourceInputID, member.SourceSegment, member.SourceSegmentQuote,
					member.SourceSpan, member.SourceGenerationID, seedSegments)) {
				return errors.New("duplicate member has missing segment provenance")
			}
			seen[member.ID] = true
			versions[member.Target.ContentVersionID] = true
		}
	}
	return nil
}

func sameConnectionRendition(source, segment document.PassageRefV1) bool {
	return source.Version == segment.Version && source.FederationDomainUID == segment.FederationDomainUID &&
		source.VaultUID == segment.VaultUID && source.DocumentUID == segment.DocumentUID &&
		source.ContentVersionID == segment.ContentVersionID && source.SourceSHA256 == segment.SourceSHA256 &&
		source.RenditionBuildID == segment.RenditionBuildID && source.AttachmentID == segment.AttachmentID &&
		source.BodySHA256 == segment.BodySHA256
}

func sameSeedSegment(inputID string, ref *document.PassageRefV1, quote string, span *document.ChunkSpan,
	generationID string, selected map[string]api.ConnectionSeedSegment,
) bool {
	segment, ok := selected[inputID]
	return ok && ref != nil && span != nil && segment.Passage == *ref && segment.Quote == quote &&
		segment.Span == *span && segment.GenerationID == generationID
}

func validConnectionQuote(ref document.PassageRefV1, quote string) bool {
	if document.ValidatePassageIdentityV1(ref) != nil || len(quote) != ref.ByteEnd-ref.ByteStart {
		return false
	}
	hash := sha256.Sum256([]byte(quote))
	return hex.EncodeToString(hash[:]) == ref.QuoteSHA256
}
