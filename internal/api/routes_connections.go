package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// The MCP adapter includes both structured content and an escaped text copy.
// Keep the HTTP report well below its 1 MiB envelope cap too.
const maxConnectionSuggestionReportBytes = 240 << 10

// ConnectionSuggestionRequest names one exact retained seed passage. A caller
// must explicitly ask for a non-semantic method; ordinary requests use only
// stored chunk vectors and never authorize a provider call.
type ConnectionSuggestionRequest struct {
	Source    document.PassageRefV1 `json:"source"`
	SeedKind  string                `json:"seed_kind,omitzero" enum:"passage,document"`
	Profile   string                `json:"profile" minLength:"1" maxLength:"128"`
	BindingID string                `json:"binding_id" minLength:"1" maxLength:"128"`
	Method    string                `json:"method,omitzero" enum:"semantic,lexical,tag,hybrid"`
	Limit     int                   `json:"limit,omitzero" minimum:"1" maximum:"100"`
	Fence     DocumentSourceFence   `json:"fence"`
}

type ConnectionSuggestion struct {
	ID                 string                      `json:"id"`
	Method             string                      `json:"method"`
	Reason             string                      `json:"reason"`
	Source             document.PassageRefV1       `json:"source"`
	SourceQuote        string                      `json:"source_quote"`
	SourceSegment      *document.PassageRefV1      `json:"source_segment,omitzero"`
	SourceSegmentQuote string                      `json:"source_segment_quote,omitzero"`
	SourceInputID      string                      `json:"source_input_id,omitzero"`
	SourceSpan         *document.ChunkSpan         `json:"source_span,omitzero"`
	SourceGeneration   string                      `json:"source_generation_id,omitzero"`
	Target             document.PassageRefV1       `json:"target"`
	TargetQuote        string                      `json:"target_quote"`
	TargetNodeID       int64                       `json:"target_node_id"`
	TargetPath         string                      `json:"target_path"`
	Score              float64                     `json:"score"`
	ScoreMetric        string                      `json:"score_metric"`
	Aggregation        string                      `json:"aggregation"`
	VectorSpaceID      string                      `json:"vector_space_id,omitzero"`
	EmbeddingSetID     string                      `json:"embedding_set_id,omitzero"`
	InputGenerationID  string                      `json:"input_generation_id,omitzero"`
	InputID            string                      `json:"input_id,omitzero"`
	TargetSpan         *document.ChunkSpan         `json:"target_span,omitzero"`
	IndexGenerationID  string                      `json:"index_generation_id,omitzero"`
	DuplicateCount     int                         `json:"duplicate_count"`
	DuplicateMembers   []ConnectionDuplicateMember `json:"duplicate_members" maxItems:"4095"`
}

// ConnectionSeedSegment identifies a stored input selected by the source
// passage or document seed. A truncated report may omit further inputs.
type ConnectionSeedSegment struct {
	InputID      string                `json:"input_id"`
	Passage      document.PassageRefV1 `json:"passage"`
	Quote        string                `json:"quote"`
	Span         document.ChunkSpan    `json:"span"`
	GenerationID string                `json:"generation_id"`
}

// ConnectionDuplicateMember is an individually verified, fenced rendition
// suppressed by content grouping. Its score and pair inputs remain visible.
type ConnectionDuplicateMember struct {
	ID                 string                 `json:"id"`
	Target             document.PassageRefV1  `json:"target"`
	TargetQuote        string                 `json:"target_quote"`
	TargetNodeID       int64                  `json:"target_node_id"`
	TargetPath         string                 `json:"target_path"`
	Score              float64                `json:"score"`
	ScoreMetric        string                 `json:"score_metric"`
	SourceSegment      *document.PassageRefV1 `json:"source_segment,omitzero"`
	SourceSegmentQuote string                 `json:"source_segment_quote,omitzero"`
	SourceInputID      string                 `json:"source_input_id,omitzero"`
	SourceSpan         *document.ChunkSpan    `json:"source_span,omitzero"`
	SourceGenerationID string                 `json:"source_generation_id,omitzero"`
	VectorSpaceID      string                 `json:"vector_space_id,omitzero"`
	EmbeddingSetID     string                 `json:"embedding_set_id,omitzero"`
	InputGenerationID  string                 `json:"input_generation_id,omitzero"`
	InputID            string                 `json:"input_id,omitzero"`
	TargetSpan         *document.ChunkSpan    `json:"target_span,omitzero"`
	IndexGenerationID  string                 `json:"index_generation_id,omitzero"`
}

type ConnectionSuggestionReport struct {
	State                  string                  `json:"state" enum:"ready,unavailable"`
	CoverageReason         string                  `json:"coverage_reason,omitzero"`
	Source                 document.PassageRefV1   `json:"source"`
	SeedKind               string                  `json:"seed_kind"`
	Method                 string                  `json:"method"`
	CandidateCount         int                     `json:"candidate_count"`
	SeedSegmentCount       int                     `json:"seed_segment_count"`
	SeedSegments           []ConnectionSeedSegment `json:"seed_segments" maxItems:"4096"`
	Aggregation            string                  `json:"aggregation,omitzero"`
	ScoreMetric            string                  `json:"score_metric,omitzero"`
	VectorSpaceID          string                  `json:"vector_space_id,omitzero"`
	IndexGenerationID      string                  `json:"index_generation_id,omitzero"`
	SourceManifestChecksum string                  `json:"source_manifest_checksum,omitzero"`
	SourceEmbeddingSetID   string                  `json:"source_embedding_set_id,omitzero"`
	Truncated              bool                    `json:"truncated"`
	Candidates             []ConnectionSuggestion  `json:"candidates"`
	FallbackMethods        []string                `json:"fallback_methods"`
}

// RegisterConnectionCandidateRoutes adds the candidate operation.
func RegisterConnectionCandidateRoutes(api huma.API, d Deps) {
	type output struct{ Body ConnectionSuggestionReport }
	huma.Register(api, huma.Operation{OperationID: "suggestConnections", Method: http.MethodPost,
		Path: "/api/v1/connections/suggest", Summary: "Suggest exact passage connections from stored evidence",
		MaxBodyBytes: 1 << 20}, func(ctx context.Context, input *struct{ Body ConnectionSuggestionRequest }) (*output, error) {
		report, err := SuggestConnections(ctx, d, input.Body)
		if err != nil {
			return nil, err
		}
		return &output{Body: report}, nil
	})
}

func SuggestConnections(ctx context.Context, d Deps, request ConnectionSuggestionRequest) (ConnectionSuggestionReport, error) {
	if d.Processing == nil || d.Store == nil || d.Blobs == nil {
		return ConnectionSuggestionReport{}, processingUnavailable()
	}
	if request.Method == "" {
		request.Method = "semantic"
	}
	if request.SeedKind == "" {
		request.SeedKind = "passage"
	}
	if request.Limit == 0 {
		request.Limit = 10
	}
	if request.Limit < 1 || request.Limit > 100 || !slices.Contains([]string{"semantic", "lexical", "tag", "hybrid"}, request.Method) ||
		!slices.Contains([]string{"passage", "document"}, request.SeedKind) {
		return ConnectionSuggestionReport{}, NewError(http.StatusUnprocessableEntity, "validation", "Invalid suggestion method or limit.")
	}
	if !slices.Contains(request.Fence.ContentVersionIDs, request.Source.ContentVersionID) ||
		request.Fence.VaultUID != d.Store.VaultID() || request.Source.VaultUID != d.Store.VaultID() {
		return ConnectionSuggestionReport{}, NewError(http.StatusUnprocessableEntity, "validation", "The exact source must be in the selected vault fence.")
	}
	report := ConnectionSuggestionReport{State: "unavailable", Source: request.Source, SeedKind: request.SeedKind, Method: request.Method,
		Candidates: []ConnectionSuggestion{}, SeedSegments: []ConnectionSeedSegment{}, FallbackMethods: []string{"lexical"}}
	if request.SeedKind == "document" {
		report.FallbackMethods = []string{}
	}
	// DF-11's tag neighborhood is optional. An unavailable requested method is
	// reported plainly; it is never relabeled as semantic evidence.
	seed, err := d.Processing.ResolvePassage(ctx, processing.PassageResolveRequest{Ref: request.Source})
	if err != nil {
		return ConnectionSuggestionReport{}, passageResolveError(err)
	}
	seedAuthority, err := d.Store.ResolvePassageAuthority(ctx, request.Source)
	if err != nil {
		if errors.Is(err, store.ErrPassageAuthorityUnavailable) || errors.Is(err, store.ErrDocumentIdentityUnavailable) ||
			errors.Is(err, store.ErrNotFound) {
			return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source changed during suggestion.")
		}
		return ConnectionSuggestionReport{}, err
	}
	if !seedAuthority.Fresh {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source version changed.")
	}
	if request.Method == "lexical" && request.SeedKind == "document" {
		report.CoverageReason = "document_lexical_unavailable"
		return report, nil
	}
	if request.Method == "tag" || request.Method == "hybrid" {
		if request.Method == "tag" {
			report.CoverageReason = "requested_method_unavailable"
		} else {
			report.CoverageReason = "tag_lane_unavailable"
		}
		return report, nil
	}
	if request.Method == "lexical" {
		return suggestLexicalConnections(ctx, d, request, report, seed.Text, seedAuthority)
	}
	coverage, err := d.Processing.Coverage(ctx, request.Profile, processing.SourceFence{
		VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs})
	if err != nil {
		return ConnectionSuggestionReport{}, fromProcessingError(err)
	}
	profile := coverage.ProfileFingerprint
	selected, err := d.Store.ActiveConnectionGeneration(ctx, seedAuthority.Node.ID, request.Source.ContentVersionID, profile, request.BindingID)
	if errors.Is(err, store.ErrSimilarSourceUnavailable) {
		report.CoverageReason = "missing_seed_embedding"
		return report, nil
	}
	if err != nil {
		return ConnectionSuggestionReport{}, fromProcessingError(err)
	}
	if selected.BuildID != request.Source.RenditionBuildID || selected.AttachmentID != request.Source.AttachmentID ||
		selected.SourceSHA256 != request.Source.SourceSHA256 {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source rendition changed.")
	}
	generation, err := readConnectionGeneration(ctx, d, selected.BlobHash)
	if err != nil {
		return ConnectionSuggestionReport{}, err
	}
	seedBody, err := readConnectionBody(ctx, d, processing.Selector{NodeID: seedAuthority.Node.ID,
		ContentVersionID: request.Source.ContentVersionID, Profile: request.Profile})
	if errors.Is(err, store.ErrNotFound) {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source changed during suggestion.")
	}
	if err != nil {
		return ConnectionSuggestionReport{}, err
	}
	start, end := request.Source.ByteStart, request.Source.ByteEnd
	if request.SeedKind == "document" {
		// The caller explicitly selected all retained chunk inputs for this
		// document version. The passage ref remains the exact identity anchor.
		start, end = 0, len(seedBody.bytes)
	}
	inputs := overlappingConnectionInputs(generation.Inputs, seedBody, start, end)
	if len(inputs) == 0 {
		report.CoverageReason = "seed_passage_not_covered"
		return report, nil
	}
	const maxSeedEvidenceBytes = 1 << 20
	if len(inputs) > 4096 {
		report.CoverageReason = "seed_evidence_too_large"
		return report, nil
	}
	inputIDs := make([]string, 0, len(inputs))
	seedBytes := 0
	for _, input := range inputs {
		first, last, ok := seedBody.locateInput(input)
		if !ok || last-first > processing.DefaultPassageReadBytes || seedBytes+last-first > maxSeedEvidenceBytes {
			report.CoverageReason = "seed_evidence_too_large"
			report.SeedSegments = []ConnectionSeedSegment{}
			return report, nil
		}
		segment, err := document.NewPassageRefV1(request.Source, seedBody.bytes, first, last)
		if err != nil {
			return ConnectionSuggestionReport{}, err
		}
		report.SeedSegments = append(report.SeedSegments, ConnectionSeedSegment{
			InputID: input.Key, Passage: segment, Quote: input.Content,
			Span: input.SourceSpan, GenerationID: selected.InputGenerationID})
		inputIDs = append(inputIDs, input.Key)
		seedBytes += last - first
	}
	report.SeedSegmentCount = len(report.SeedSegments)
	resolution, metric, indexGeneration, err := d.Store.SearchConnectionCandidates(ctx, profile, request.BindingID,
		store.SimilarSource{NodeID: seedAuthority.Node.ID, ContentVersionID: request.Source.ContentVersionID, InputIDs: inputIDs},
		request.Limit, store.SearchOptions{ContentVersionIDs: request.Fence.ContentVersionIDs})
	if errors.Is(err, store.ErrSimilarSourceUnavailable) {
		report.CoverageReason = "missing_seed_embedding"
		return report, nil
	}
	if errors.Is(err, store.ErrVectorIndexSourceStale) || errors.Is(err, store.ErrProcessingSourceFenceStaleVersion) || errors.Is(err, store.ErrNotFound) {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source or vector index changed.")
	}
	if err != nil {
		return ConnectionSuggestionReport{}, fromProcessingError(err)
	}
	report.State, report.Truncated = "ready", resolution.Truncated
	report.Aggregation, report.ScoreMetric = "maximum_pair_score", metric
	report.VectorSpaceID, report.IndexGenerationID = selected.VectorSpaceID, indexGeneration
	report.SourceManifestChecksum, report.SourceEmbeddingSetID = resolution.SourceManifestChecksum, selected.EmbeddingSetID
	for _, item := range resolution.Candidates {
		candidate, err := materializeConnectionCandidate(ctx, d, request, profile, selected.VectorSpaceID,
			metric, indexGeneration, item, seed.Text, seedBody, generation, selected.InputGenerationID)
		if errors.Is(err, store.ErrSimilarSourceUnavailable) || errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return ConnectionSuggestionReport{}, err
		}
		candidate.DuplicateMembers = make([]ConnectionDuplicateMember, 0, len(item.DuplicateMembers))
		for _, member := range item.DuplicateMembers {
			matched := store.SimilarSearchCandidate{SemanticSearchCandidate: member.SemanticSearchCandidate,
				SourceInputID: member.SourceInputID}
			verified, memberErr := materializeConnectionCandidate(ctx, d, request, profile, selected.VectorSpaceID,
				metric, indexGeneration, matched, seed.Text, seedBody, generation, selected.InputGenerationID)
			if errors.Is(memberErr, store.ErrSimilarSourceUnavailable) || errors.Is(memberErr, store.ErrNotFound) {
				continue
			}
			if memberErr != nil {
				return ConnectionSuggestionReport{}, memberErr
			}
			candidate.DuplicateMembers = append(candidate.DuplicateMembers, connectionDuplicateMember(verified))
		}
		candidate.DuplicateCount = len(candidate.DuplicateMembers)
		report.Candidates = append(report.Candidates, candidate)
	}
	// All counts are based only on successfully materialized, scoped candidates.
	report.CandidateCount = len(report.Candidates)
	current, err := d.Store.ResolvePassageAuthority(ctx, request.Source)
	if err != nil || !current.Fresh || current.Node.Revision != seedAuthority.Node.Revision {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source changed during suggestion.")
	}
	latest, err := d.Store.ActiveConnectionGeneration(ctx, seedAuthority.Node.ID,
		request.Source.ContentVersionID, profile, request.BindingID)
	if err != nil || latest != selected {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source embedding changed during suggestion.")
	}
	return boundConnectionSuggestionReport(report)
}

func suggestLexicalConnections(ctx context.Context, d Deps, request ConnectionSuggestionRequest,
	report ConnectionSuggestionReport, quote string, seed store.PassageAuthority,
) (ConnectionSuggestionReport, error) {
	if quote == "" {
		report.CoverageReason = "empty_seed_passage"
		return report, nil
	}
	found, err := d.Processing.Search(ctx, processing.SearchRequest{Query: quote, Mode: "lexical",
		Profile: request.Profile, Limit: 100, Fence: processing.SourceFence{
			VaultUID: request.Fence.VaultUID, ContentVersionIDs: request.Fence.ContentVersionIDs}})
	if err != nil {
		return ConnectionSuggestionReport{}, fromProcessingError(err)
	}
	seen := make(map[string]int)
	report.State, report.Truncated = "ready", found.Truncated
	report.Aggregation, report.ScoreMetric = "exact_quote", "reciprocal_lexical_rank"
	for _, hit := range found.Results {
		if hit.Document.NodeID == seed.Node.ID || hit.LexicalRank < 1 {
			continue
		}
		version, err := d.Store.ContentVersionByID(ctx, hit.Document.ContentVersionID)
		if err != nil || version.NodeID != hit.Document.NodeID {
			continue
		}
		body, err := readConnectionBody(ctx, d, processing.Selector{NodeID: hit.Document.NodeID,
			ContentVersionID: hit.Document.ContentVersionID, Profile: request.Profile})
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return ConnectionSuggestionReport{}, err
		}
		start := bytes.Index(body.bytes, []byte(quote))
		if start < 0 {
			continue
		}
		identity, err := d.Store.EnsureDocumentIdentity(ctx, hit.Document.NodeID)
		if err != nil {
			return ConnectionSuggestionReport{}, err
		}
		target, err := document.NewPassageRefV1(document.PassageRefV1{VaultUID: d.Store.VaultID(),
			DocumentUID: identity.DocumentUID, ContentVersionID: hit.Document.ContentVersionID,
			SourceSHA256: version.BlobHash, RenditionBuildID: body.rendition.BuildID,
			AttachmentID: body.rendition.AttachmentID}, body.bytes, start, start+len(quote))
		if err != nil {
			return ConnectionSuggestionReport{}, err
		}
		id, err := connectionCandidateID(request.Source, target, "lexical", "exact_quote")
		if err != nil {
			return ConnectionSuggestionReport{}, err
		}
		if index, exists := seen[version.BlobHash]; exists {
			appendLexicalDuplicate(&report.Candidates[index], quote, body, ConnectionDuplicateMember{
				ID: id, Target: target, TargetQuote: quote, TargetNodeID: hit.Document.NodeID,
				TargetPath: hit.Path, Score: 1 / float64(hit.LexicalRank), ScoreMetric: "reciprocal_lexical_rank"})
			continue
		}
		if len(report.Candidates) == request.Limit {
			report.Truncated = true
			continue
		}
		seen[version.BlobHash] = len(report.Candidates)
		report.Candidates = append(report.Candidates, ConnectionSuggestion{ID: id, Method: "lexical",
			Reason: "exact_quote_overlap", Source: request.Source, SourceQuote: quote,
			Target: target, TargetQuote: quote, TargetNodeID: hit.Document.NodeID, TargetPath: hit.Path,
			Score: 1 / float64(hit.LexicalRank), ScoreMetric: "reciprocal_lexical_rank", Aggregation: "exact_quote",
			DuplicateMembers: []ConnectionDuplicateMember{}})
	}
	report.CandidateCount = len(report.Candidates)
	current, err := d.Store.ResolvePassageAuthority(ctx, request.Source)
	if err != nil || !current.Fresh || current.Node.Revision != seed.Node.Revision {
		return ConnectionSuggestionReport{}, NewError(http.StatusConflict, "stale_source", "The source changed during suggestion.")
	}
	return boundConnectionSuggestionReport(report)
}

// boundConnectionSuggestionReport keeps complete candidate groups in rank
// order. In particular, it never shortens a duplicate list to fit the wire.
// Seed evidence in a truncated report covers every returned semantic pair.
func boundConnectionSuggestionReport(report ConnectionSuggestionReport) (ConnectionSuggestionReport, error) {
	fits, err := connectionSuggestionReportFits(report)
	if err != nil || fits {
		return report, err
	}
	report.Truncated = true
	allCandidates, allSeeds := report.Candidates, report.SeedSegments
	for count := len(allCandidates); count >= 0; count-- {
		report.Candidates = allCandidates[:count]
		report.CandidateCount = count
		if report.Method == "semantic" {
			if count == 0 {
				report.SeedSegments = allSeeds[:min(1, len(allSeeds))]
			} else {
				required := make(map[string]bool)
				for _, candidate := range report.Candidates {
					required[candidate.SourceInputID] = true
					for _, duplicate := range candidate.DuplicateMembers {
						required[duplicate.SourceInputID] = true
					}
				}
				selected := make([]ConnectionSeedSegment, 0, len(required))
				for _, seed := range allSeeds {
					if required[seed.InputID] {
						selected = append(selected, seed)
					}
				}
				if len(selected) != len(required) {
					continue
				}
				report.SeedSegments = selected
			}
		}
		report.SeedSegmentCount = len(report.SeedSegments)
		fits, err = connectionSuggestionReportFits(report)
		if err != nil || fits {
			return report, err
		}
	}
	// A single seed segment can itself exceed the envelope. An explicitly
	// truncated ready result with no candidates is still a useful bounded reply.
	report.Candidates = []ConnectionSuggestion{}
	report.CandidateCount = 0
	report.SeedSegments = []ConnectionSeedSegment{}
	report.SeedSegmentCount = 0
	fits, err = connectionSuggestionReportFits(report)
	if err != nil || fits {
		return report, err
	}
	return ConnectionSuggestionReport{}, errors.New("connection report metadata exceeds the response budget")
}

func connectionSuggestionReportFits(report ConnectionSuggestionReport) (bool, error) {
	encoded, err := json.Marshal(report)
	return len(encoded) <= maxConnectionSuggestionReportBytes, err
}

func appendLexicalDuplicate(candidate *ConnectionSuggestion, quote string, body connectionBody,
	member ConnectionDuplicateMember,
) bool {
	if member.TargetQuote != quote || !bytes.Contains(body.bytes, []byte(quote)) {
		return false
	}
	candidate.DuplicateMembers = append(candidate.DuplicateMembers, member)
	candidate.DuplicateCount = len(candidate.DuplicateMembers)
	return true
}

func connectionCandidateID(source, target document.PassageRefV1, method, recipe string) (string, error) {
	sourceID, err := document.PassageIdentityV1(source)
	if err != nil {
		return "", err
	}
	targetID, err := document.PassageIdentityV1(target)
	if err != nil {
		return "", err
	}
	id := sha256.Sum256([]byte("connection-candidate/v1\x00" + sourceID + "\x00" + targetID + "\x00" + method + "\x00" + recipe))
	return hex.EncodeToString(id[:]), nil
}

func readConnectionGeneration(ctx context.Context, d Deps, hash string) (document.EmbeddingInputGeneration, error) {
	stream, size, err := d.Blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		return document.EmbeddingInputGeneration{}, err
	}
	defer func() { _ = stream.Close() }()
	const maxBytes = 64 << 20
	if size < 1 || size > maxBytes {
		return document.EmbeddingInputGeneration{}, NewError(http.StatusUnprocessableEntity, "coverage_unavailable", "Chunk generation exceeds the suggestion read budget.")
	}
	data, err := io.ReadAll(io.LimitReader(stream, maxBytes+1))
	if err != nil || int64(len(data)) != size || !stream.Verified() {
		return document.EmbeddingInputGeneration{}, NewError(http.StatusInternalServerError, "corrupt_generation", "Stored chunk generation failed verification.")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != hash {
		return document.EmbeddingInputGeneration{}, NewError(http.StatusInternalServerError, "corrupt_generation", "Stored chunk generation failed verification.")
	}
	return document.DecodeEmbeddingInputGeneration(data, document.EmbeddingInputGenerationDecodeBounds{MaxEncodedBytes: maxBytes, MaxInputs: 100000})
}

func overlappingConnectionInputs(inputs []document.GeneratedEmbeddingInput, body connectionBody, start, end int) []document.GeneratedEmbeddingInput {
	selected := make([]document.GeneratedEmbeddingInput, 0)
	for _, input := range inputs {
		first, last, ok := body.locateInput(input)
		if ok && first < end && last > start {
			selected = append(selected, input)
		}
	}
	return selected
}

func connectionDuplicateMember(candidate ConnectionSuggestion) ConnectionDuplicateMember {
	return ConnectionDuplicateMember{ID: candidate.ID, Target: candidate.Target, TargetQuote: candidate.TargetQuote,
		TargetNodeID: candidate.TargetNodeID, TargetPath: candidate.TargetPath,
		Score: candidate.Score, ScoreMetric: candidate.ScoreMetric,
		SourceSegment: candidate.SourceSegment, SourceSegmentQuote: candidate.SourceSegmentQuote,
		SourceInputID: candidate.SourceInputID, SourceSpan: candidate.SourceSpan,
		SourceGenerationID: candidate.SourceGeneration, VectorSpaceID: candidate.VectorSpaceID,
		EmbeddingSetID: candidate.EmbeddingSetID, InputGenerationID: candidate.InputGenerationID,
		InputID: candidate.InputID, TargetSpan: candidate.TargetSpan,
		IndexGenerationID: candidate.IndexGenerationID}
}

type connectionBody struct {
	bytes       []byte
	frontmatter document.RenditionFrontMatterV1
	unitKeys    []string
	navByKey    map[string]int
	rendition   processing.Rendition
}

func (body connectionBody) locateInput(input document.GeneratedEmbeddingInput) (int, int, bool) {
	if input.SourceSpan.UnitIndex < 0 || input.SourceSpan.UnitIndex >= len(body.unitKeys) {
		return 0, 0, false
	}
	index, found := body.navByKey[body.unitKeys[input.SourceSpan.UnitIndex]]
	if !found {
		return 0, 0, false
	}
	entries := body.frontmatter.Navigation.Entries[index:min(index+2, len(body.frontmatter.Navigation.Entries))]
	return document.LocateConnectionInput(body.bytes, entries, body.unitKeys, input)
}

func readConnectionBody(ctx context.Context, d Deps, selector processing.Selector) (connectionBody, error) {
	rendition, err := d.Processing.Rendition(ctx, selector, processing.MaxRenditionBytes)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return connectionBody{}, err
		}
		return connectionBody{}, fromProcessingError(err)
	}
	defer func() { _ = rendition.Reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(rendition.Reader, processing.MaxRenditionBytes+1))
	if err != nil || int64(len(data)) != rendition.Size || !rendition.Reader.Verified() {
		return connectionBody{}, NewError(http.StatusInternalServerError, "corrupt_rendition", "Stored rendition failed verification.")
	}
	frontmatter, body, err := document.ParseRenditionFrontMatterV1(data)
	if err != nil || frontmatter.Rendition.BuildID != rendition.BuildID {
		return connectionBody{}, NewError(http.StatusInternalServerError, "corrupt_rendition", "Stored rendition failed verification.")
	}
	view, err := d.Store.ActiveRendition(ctx, selector.ContentVersionID, rendition.ProfileFingerprint)
	if err != nil || view.Build.ID != rendition.BuildID || view.Attachment.ID != rendition.AttachmentID {
		return connectionBody{}, NewError(http.StatusConflict, "stale_source", "The source rendition changed.")
	}
	unitKeys := make([]string, len(view.Build.Units))
	for i, unit := range view.Build.Units {
		unitKeys[i] = unit.EvidenceUnitID
	}
	navByKey := make(map[string]int, len(frontmatter.Navigation.Entries))
	for i, entry := range frontmatter.Navigation.Entries {
		navByKey[entry.Key] = i
	}
	return connectionBody{bytes: body, frontmatter: frontmatter, unitKeys: unitKeys,
		navByKey: navByKey, rendition: rendition}, nil
}

func materializeConnectionCandidate(ctx context.Context, d Deps, request ConnectionSuggestionRequest,
	profile, sourceSpace, metric, indexGeneration string, item store.SimilarSearchCandidate,
	sourceQuote string, sourceBody connectionBody, sourceGeneration document.EmbeddingInputGeneration, sourceGenerationID string,
) (ConnectionSuggestion, error) {
	if !document.SameVectorSpace(sourceSpace, item.VectorSpaceID) {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	binding, err := d.Store.ActiveConnectionGeneration(ctx, item.NodeID, item.ContentVersionID, profile, request.BindingID)
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	if !document.SameVectorSpace(sourceSpace, binding.VectorSpaceID) ||
		binding.EmbeddingSetID != item.EmbeddingSetID || binding.InputGenerationID != item.InputGenerationID {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	var sourceInput *document.GeneratedEmbeddingInput
	for i := range sourceGeneration.Inputs {
		if sourceGeneration.Inputs[i].Key == item.SourceInputID {
			sourceInput = &sourceGeneration.Inputs[i]
			break
		}
	}
	if sourceInput == nil {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	sourceStart, sourceEnd, ok := sourceBody.locateInput(*sourceInput)
	if !ok || sourceEnd-sourceStart > processing.DefaultPassageReadBytes {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	sourceSegment, err := document.NewPassageRefV1(request.Source, sourceBody.bytes, sourceStart, sourceEnd)
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	generation, err := readConnectionGeneration(ctx, d, binding.BlobHash)
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	var matched *document.GeneratedEmbeddingInput
	for i := range generation.Inputs {
		if generation.Inputs[i].Key == item.InputID {
			matched = &generation.Inputs[i]
			break
		}
	}
	if matched == nil || matched.Content == "" {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	body, err := readConnectionBody(ctx, d, processing.Selector{NodeID: item.NodeID,
		ContentVersionID: item.ContentVersionID, Profile: request.Profile})
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	if body.rendition.AttachmentID != binding.AttachmentID || body.rendition.BuildID != binding.BuildID {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	start, end, ok := body.locateInput(*matched)
	if !ok || end-start > processing.DefaultPassageReadBytes {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	identity, err := d.Store.EnsureDocumentIdentity(ctx, item.NodeID)
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	latest, err := d.Store.ActiveConnectionGeneration(ctx, item.NodeID, item.ContentVersionID, profile, request.BindingID)
	if err != nil || latest != binding {
		return ConnectionSuggestion{}, store.ErrSimilarSourceUnavailable
	}
	target, err := document.NewPassageRefV1(document.PassageRefV1{VaultUID: d.Store.VaultID(),
		DocumentUID: identity.DocumentUID, ContentVersionID: item.ContentVersionID,
		SourceSHA256: binding.SourceSHA256, RenditionBuildID: body.rendition.BuildID,
		AttachmentID: body.rendition.AttachmentID}, body.bytes, start, end)
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	id, err := connectionCandidateID(request.Source, target, "semantic", request.BindingID+"/"+sourceSpace)
	if err != nil {
		return ConnectionSuggestion{}, err
	}
	return ConnectionSuggestion{ID: id, Method: "semantic", Reason: "stored_chunk_vector_match",
		Source: request.Source, SourceQuote: sourceQuote, SourceSegment: &sourceSegment,
		SourceSegmentQuote: sourceInput.Content, SourceInputID: item.SourceInputID,
		SourceSpan: &sourceInput.SourceSpan, SourceGeneration: sourceGenerationID,
		Target: target, TargetQuote: matched.Content,
		TargetNodeID: item.NodeID, TargetPath: item.Path, Score: item.Score, ScoreMetric: metric,
		Aggregation:   "maximum_pair_score",
		VectorSpaceID: sourceSpace, EmbeddingSetID: item.EmbeddingSetID,
		InputGenerationID: item.InputGenerationID, InputID: item.InputID, TargetSpan: &matched.SourceSpan,
		IndexGenerationID: indexGeneration, DuplicateMembers: []ConnectionDuplicateMember{}}, nil
}
