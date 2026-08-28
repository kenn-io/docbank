package retrieval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

type Backend interface {
	VaultID() string
	SearchExplainedLexicalCandidates(ctx context.Context, query string, limit int,
		options store.SearchOptions) ([]store.ExplainedLexicalCandidate, bool, error)
}

type CandidateRevalidationBackend interface {
	RevalidateSearchCandidates(ctx context.Context, candidates []store.SearchCandidateIdentity,
		options store.SearchOptions, semanticProfileFingerprint,
		semanticBindingID string) (store.SearchCandidateRevalidation, error)
}

var ErrExpandedSearchFailed = errors.New("expanded retrieval stage failed")

type SemanticBackend interface {
	AcquireSemanticSearchAuthority(ctx context.Context, profile, binding, owner string, at time.Time,
		duration time.Duration, options store.SearchOptions) (store.SemanticSearchAuthority, error)
	ResolveSemanticCandidates(ctx context.Context, profile, binding string, inputKind document.EmbeddingInputKind,
		vectorSpace, sourceManifest string, neighbors []vectorindex.Neighbor, limit int,
		options store.SearchOptions) (store.SemanticSearchResolution, error)
	ReleaseVectorIndexGeneration(ctx context.Context, leaseID string, fencingToken int64, at time.Time) error
}

type QueryEncoderResolver interface {
	ResolveQueryEncoder(ctx context.Context, descriptor document.EmbeddingDescriptor) (document.EmbeddingProvider, error)
}

// QueryEmbeddingOperation is the complete immutable identity presented to the
// consent boundary immediately before one private query is sent to an encoder.
type QueryEmbeddingOperation struct {
	ProviderID            string
	DescriptorFingerprint string
	PolicyFingerprint     string
	ProfileFingerprint    string
	DisclosureFingerprint string
	Scope                 store.SearchOptions
	InputClass            ProviderInputClass
}

// QueryEmbeddingAuthorizer rechecks current consent for one exact query
// embedding operation. A prior check is never a bearer capability.
type QueryEmbeddingAuthorizer interface {
	AuthorizeQueryEmbedding(ctx context.Context, operation QueryEmbeddingOperation) error
}

type authorizedQueryEncoder struct {
	provider   document.EmbeddingProvider
	authorizer QueryEmbeddingAuthorizer
	operation  QueryEmbeddingOperation
}

func (encoder authorizedQueryEncoder) Descriptor() document.EmbeddingDescriptor {
	return encoder.provider.Descriptor()
}

func (encoder authorizedQueryEncoder) Embed(ctx context.Context, inputs []document.EmbeddingInput,
	authorization document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	if err := encoder.authorizer.AuthorizeQueryEmbedding(ctx, encoder.operation); err != nil {
		return document.EmbeddingResult{}, err
	}
	return encoder.provider.Embed(ctx, inputs, authorization)
}

type SearcherConfig struct {
	Backend                  Backend
	Encoders                 QueryEncoderResolver
	QueryEmbeddingAuthorizer QueryEmbeddingAuthorizer
	Owner                    string
	LeaseDuration            time.Duration
	Clock                    func() time.Time
	Expansion                ExpansionConfig
	Reranking                RerankingConfig
}

type Searcher struct {
	backend                  Backend
	encoders                 QueryEncoderResolver
	queryEmbeddingAuthorizer QueryEmbeddingAuthorizer
	owner                    string
	leaseDuration            time.Duration
	clock                    func() time.Time
	expansion                ExpansionConfig
	reranking                RerankingConfig
}

func NewSearcher(config SearcherConfig) (*Searcher, error) {
	if config.Backend == nil {
		return nil, errors.New("retrieval backend is required")
	}
	if config.Owner == "" {
		return nil, errors.New("retrieval owner is required")
	}
	if config.LeaseDuration <= 0 {
		return nil, errors.New("retrieval vector lease duration must be positive")
	}
	if (config.Encoders == nil) != (config.QueryEmbeddingAuthorizer == nil) {
		return nil, errors.New("retrieval query encoder and query embedding authorizer must be configured together")
	}
	if err := validateExpansionConfig(config.Expansion); err != nil {
		return nil, err
	}
	if err := validateRerankingConfig(config.Reranking); err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Searcher{backend: config.Backend, encoders: config.Encoders,
		queryEmbeddingAuthorizer: config.QueryEmbeddingAuthorizer, owner: config.Owner,
		leaseDuration: config.LeaseDuration, clock: config.Clock, expansion: config.Expansion,
		reranking: config.Reranking}, nil
}

func (searcher *Searcher) Search(ctx context.Context, query Query) (Report, error) {
	query, err := normalizeQuery(query)
	if err != nil {
		return Report{}, err
	}
	variants, expansionReceipt, expansionDegradation, err := searcher.expand(ctx, query)
	if err != nil {
		return Report{}, err
	}
	reports := make([]Report, 0, len(variants)+1)
	for _, text := range append([]string{query.Text}, variants...) {
		variant := query
		variant.Text = text
		report, err := searcher.searchBase(ctx, variant)
		if err != nil {
			if searcher.expansion.Enabled {
				if ctx.Err() != nil {
					return Report{}, ctx.Err()
				}
				return Report{}, ErrExpandedSearchFailed
			}
			return Report{}, err
		}
		reports = append(reports, report)
	}
	report, err := mergeVariantReports(reports, query.Limit)
	if err != nil {
		return Report{}, err
	}
	if expansionReceipt != nil {
		report.Receipts = append(report.Receipts, *expansionReceipt)
	}
	if expansionDegradation != DegradationNone {
		report.Degradations = append(report.Degradations, expansionDegradation)
	}
	if searcher.expansion.Enabled {
		revalidated, err := searcher.revalidateExpandedReport(ctx, query, report)
		if err != nil {
			if ctx.Err() != nil {
				return Report{}, ctx.Err()
			}
			return Report{}, ErrExpandedSearchFailed
		}
		report = revalidated
	}
	report, rerankingReceipt, rerankingDegradation, err := searcher.rerank(ctx, query, report)
	if err != nil {
		return Report{}, err
	}
	if rerankingReceipt != nil {
		report.Receipts = append(report.Receipts, *rerankingReceipt)
	}
	if rerankingDegradation != DegradationNone {
		report.Degradations = append(report.Degradations, rerankingDegradation)
	}
	return report, nil
}

func (searcher *Searcher) revalidateExpandedReport(ctx context.Context, query Query, report Report) (Report, error) {
	backend, ok := searcher.backend.(CandidateRevalidationBackend)
	if !ok {
		return Report{}, errors.New("expanded retrieval revalidation is unavailable")
	}
	requested := make([]store.SearchCandidateIdentity, len(report.Results))
	for i, result := range report.Results {
		evidence := make([]store.SearchEvidenceIdentity, len(result.Evidence))
		var nodeRevision int64
		for j, reference := range result.Evidence {
			if nodeRevision == 0 {
				nodeRevision = reference.NodeRevision
			}
			if reference.NodeRevision != nodeRevision {
				return Report{}, errors.New("search result evidence spans node revisions")
			}
			evidence[j] = store.SearchEvidenceIdentity{Kind: reference.Kind,
				VectorSpaceID: reference.VectorSpaceID, EmbeddingSetID: reference.EmbeddingSetID,
				InputGenerationID: reference.InputGenerationID, InputID: reference.InputID,
				InputKind: reference.InputKind, BuildID: reference.BuildID, SegmentID: reference.SegmentID,
				BlobHash: reference.BlobHash, SourceManifestChecksum: reference.SourceManifestChecksum}
		}
		requested[i] = store.SearchCandidateIdentity{NodeID: result.Document.NodeID, NodeRevision: nodeRevision,
			ContentVersionID: result.Document.ContentVersionID, Evidence: evidence}
	}
	semanticProfile, semanticBinding := "", ""
	if report.ActualMode == ModeSemantic || report.ActualMode == ModeHybrid {
		semanticProfile, semanticBinding = query.ProcessingProfileFingerprint, query.BindingID
	}
	revalidation, err := backend.RevalidateSearchCandidates(ctx, requested, query.Scope,
		semanticProfile, semanticBinding)
	if err != nil {
		return Report{}, err
	}
	if revalidation.Coverage != nil {
		report.Coverage.ScopedDocuments = revalidation.Coverage.ScopedDocuments
		report.Coverage.CompleteDocuments = revalidation.Coverage.CompleteDocuments
		report.Coverage.State = CoverageComplete
		if report.Coverage.ScopedDocuments != report.Coverage.CompleteDocuments {
			report.Coverage.State = CoverageIncomplete
			if report.RequestedMode == ModeAuto && report.Coverage.BindingRequired {
				return Report{}, errors.New("required semantic coverage changed during expanded retrieval")
			}
		}
	}
	type documentKey struct {
		nodeID  int64
		version string
	}
	set := make(map[documentKey]struct{}, len(revalidation.Candidates))
	for _, candidate := range revalidation.Candidates {
		set[documentKey{nodeID: candidate.NodeID, version: candidate.ContentVersionID}] = struct{}{}
	}
	results := report.Results[:0]
	for _, result := range report.Results {
		if _, ok := set[documentKey{nodeID: result.Document.NodeID,
			version: result.Document.ContentVersionID}]; ok {
			results = append(results, result)
		}
	}
	if len(results) != len(report.Results) {
		report.Truncated = true
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	report.Results = results
	return report, nil
}

func (searcher *Searcher) searchBase(ctx context.Context, query Query) (Report, error) {
	requested := query.Mode
	switch requested {
	case ModeLexical:
		return searcher.lexical(ctx, query, requested, DegradationNone, Coverage{State: CoverageUnknown})
	case ModeSemantic:
		semantic, coverage, truncated, err := searcher.semantic(ctx, query, false)
		if err != nil {
			return Report{}, err
		}
		return laneReport(requested, ModeSemantic, coverage, DegradationNone, semantic, truncated), nil
	case ModeHybrid:
		return searcher.hybrid(ctx, query, requested)
	case ModeAuto:
		semantic, coverage, semanticTruncated, semanticErr := searcher.semantic(ctx, query, true)
		if semanticErr != nil {
			degradable := &semanticDegradationError{}
			ok := errors.As(semanticErr, &degradable)
			if !ok {
				return Report{}, semanticErr
			}
			return searcher.lexical(ctx, query, requested, degradable.degradation, coverage)
		}
		lexical, lexicalTruncated, err := searcher.collectLexical(ctx, query)
		if err != nil {
			return Report{}, err
		}
		return makeHybridReport(requested, coverage, lexical, semantic,
			lexicalTruncated || semanticTruncated, query.Limit)
	}
	return Report{}, errors.New("unreachable retrieval mode")
}

func mergeVariantReports(reports []Report, limit int) (Report, error) {
	if len(reports) == 0 {
		return Report{}, errors.New("retrieval requires one query report")
	}
	if len(reports) == 1 {
		return reports[0], nil
	}
	merged := reports[0]
	merged.Coverage = conservativeCoverage(reports)
	byDocument := make(map[DocumentIdentity]*Result)
	for _, report := range reports {
		if report.RequestedMode != merged.RequestedMode || report.ActualMode != merged.ActualMode {
			return Report{}, errors.New("query variants returned inconsistent retrieval modes")
		}
		if report.Degradation != merged.Degradation || !slices.Equal(report.Degradations, merged.Degradations) {
			return Report{}, errors.New("query variants returned incompatible degradations")
		}
		merged.Truncated = merged.Truncated || report.Truncated
		for _, result := range report.Results {
			current := byDocument[result.Document]
			if current == nil {
				mergedResult := result
				mergedResult.Explanation = slices.Clone(result.Explanation)
				mergedResult.Evidence = slices.Clone(result.Evidence)
				byDocument[result.Document] = &mergedResult
				continue
			}
			current.Score += result.Score
			current.Explanation = append(current.Explanation, result.Explanation...)
			current.Evidence = append(current.Evidence, result.Evidence...)
		}
	}
	merged.Results = make([]Result, 0, len(byDocument))
	for _, result := range byDocument {
		merged.Results = append(merged.Results, *result)
	}
	slices.SortFunc(merged.Results, compareResults)
	if len(merged.Results) > limit {
		merged.Results = merged.Results[:limit]
		merged.Truncated = true
	}
	for index := range merged.Results {
		merged.Results[index].Rank = index + 1
	}
	return merged, nil
}

func conservativeCoverage(reports []Report) Coverage {
	coverage := reports[0].Coverage
	complete := coverage.CompleteDocuments
	unknown, incomplete := coverage.State == CoverageUnknown, coverage.State == CoverageIncomplete
	for _, report := range reports[1:] {
		coverage.BindingRequired = coverage.BindingRequired || report.Coverage.BindingRequired
		coverage.ScopedDocuments = max(coverage.ScopedDocuments, report.Coverage.ScopedDocuments)
		complete = min(complete, report.Coverage.CompleteDocuments)
		unknown = unknown || report.Coverage.State == CoverageUnknown
		incomplete = incomplete || report.Coverage.State == CoverageIncomplete
	}
	if unknown {
		coverage.ScopedDocuments = 0
		coverage.CompleteDocuments = 0
		coverage.State = CoverageUnknown
		return coverage
	}
	coverage.CompleteDocuments = complete
	if incomplete || coverage.CompleteDocuments != coverage.ScopedDocuments {
		coverage.State = CoverageIncomplete
		return coverage
	}
	coverage.State = CoverageComplete
	return coverage
}

type semanticDegradationError struct {
	degradation Degradation
	cause       error
}

type semanticReleaseError struct {
	operation error
	release   error
}

func (failure *semanticReleaseError) Error() string {
	return fmt.Sprintf("%v; releasing semantic generation: %v", failure.operation, failure.release)
}

func (failure *semanticReleaseError) Unwrap() error { return failure.release }

func (failure *semanticDegradationError) Error() string { return failure.cause.Error() }
func (failure *semanticDegradationError) Unwrap() error { return failure.cause }

func degradableSemanticFailure(degradation Degradation, cause error) error {
	return &semanticDegradationError{degradation: degradation, cause: cause}
}

func (searcher *Searcher) hybrid(ctx context.Context, query Query, requested Mode) (Report, error) {
	lexical, lexicalTruncated, err := searcher.collectLexical(ctx, query)
	if err != nil {
		return Report{}, err
	}
	semantic, coverage, semanticTruncated, err := searcher.semantic(ctx, query, false)
	if err != nil {
		return Report{}, err
	}
	return makeHybridReport(requested, coverage, lexical, semantic,
		lexicalTruncated || semanticTruncated, query.Limit)
}

func (searcher *Searcher) semantic(ctx context.Context, query Query,
	degradeIncompleteRequired bool,
) (_ []Candidate, coverage Coverage, truncated bool, retErr error) {
	backend, ok := searcher.backend.(SemanticBackend)
	if !ok || searcher.encoders == nil {
		return nil, Coverage{State: CoverageUnknown}, false, degradableSemanticFailure(
			DegradationSemanticUnavailable, errors.New("semantic retrieval is not configured"))
	}
	authority, err := backend.AcquireSemanticSearchAuthority(ctx,
		query.ProcessingProfileFingerprint, query.BindingID, searcher.owner,
		searcher.clock().UTC(), searcher.leaseDuration, query.Scope)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, Coverage{State: CoverageUnknown}, false, degradableSemanticFailure(
				DegradationSemanticUnavailable, err)
		}
		return nil, Coverage{State: CoverageUnknown}, false, err
	}
	defer func() {
		if releaseErr := backend.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx), authority.Lease.ID,
			authority.Lease.FencingToken, searcher.clock().UTC()); releaseErr != nil {
			if retErr == nil {
				retErr = releaseErr
			} else {
				// A failed release makes the whole operation non-degradable even
				// when the earlier provider stage could have fallen back safely.
				retErr = &semanticReleaseError{operation: retErr, release: releaseErr}
			}
		}
	}()
	coverage = Coverage{BindingRequired: authority.BindingRequired,
		ScopedDocuments: authority.ScopedDocuments, CompleteDocuments: authority.CompleteDocuments,
		State: CoverageComplete}
	if authority.CompleteDocuments != authority.ScopedDocuments {
		coverage.State = CoverageIncomplete
		if authority.BindingRequired && degradeIncompleteRequired {
			return nil, coverage, false, degradableSemanticFailure(DegradationIncompleteCoverage,
				errors.New("required semantic coverage is incomplete"))
		}
	}
	if authority.ANNRows == nil {
		return nil, coverage, false, errors.New("semantic search authority lacks source-fenced ANN rows")
	}
	provider, err := searcher.encoders.ResolveQueryEncoder(ctx, authority.VectorSpace.Descriptor)
	if err != nil {
		return nil, coverage, false, degradableSemanticFailure(DegradationProviderUnavailable, err)
	}
	if provider == nil {
		return nil, coverage, false, degradableSemanticFailure(DegradationSemanticUnavailable,
			errors.New("query encoder runtime is unavailable"))
	}
	if !reflect.DeepEqual(provider.Descriptor(), authority.VectorSpace.Descriptor) {
		return nil, coverage, false, errors.New("query encoder does not reproduce the active vector-space descriptor")
	}
	if err := document.ValidateEmbeddingQueryCompatibility(authority.VectorSpace.Descriptor,
		provider.Descriptor()); err != nil {
		return nil, coverage, false, err
	}
	inputs := []document.EmbeddingInput{{Key: "query", Role: document.EmbeddingRoleQuery,
		Kind: document.EmbeddingInputQueryText, Text: query.Text}}
	operation := QueryEmbeddingOperation{ProviderID: authority.VectorSpace.Descriptor.ID,
		DescriptorFingerprint: authority.VectorSpace.Descriptor.Fingerprint,
		PolicyFingerprint:     authority.VectorSpace.Descriptor.PolicyFingerprint,
		ProfileFingerprint:    query.ProcessingProfileFingerprint,
		DisclosureFingerprint: authority.DisclosureFingerprint,
		Scope:                 query.Scope, InputClass: ProviderInputQueryText}
	embedded, err := document.ExecuteEmbedding(ctx, authorizedQueryEncoder{
		provider: provider, authorizer: searcher.queryEmbeddingAuthorizer, operation: operation,
	}, inputs, query.Authorization)
	if err != nil {
		return nil, coverage, false, degradableSemanticFailure(DegradationProviderUnavailable, err)
	}
	stored := authority.Lease.Generation
	generation, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
	if err != nil {
		return nil, coverage, false, err
	}
	metadata := generation.Metadata()
	descriptor := authority.VectorSpace.Descriptor
	if metadata.VectorSpaceID != authority.VectorSpace.ID || metadata.Dimension != descriptor.Dimension ||
		metadata.Metric != descriptor.Metric || metadata.Normalization != descriptor.Normalization ||
		metadata.Manifest.Checksum != stored.IndexManifestChecksum || metadata.RowCount != stored.RowCount {
		return nil, coverage, false, errors.New("leased vector generation is incompatible with active query authority")
	}
	neighbors, err := generation.SearchRows(embedded.Vectors[0].Values, authority.ANNRows)
	if err != nil {
		return nil, coverage, false, err
	}
	resolution, err := backend.ResolveSemanticCandidates(ctx, query.ProcessingProfileFingerprint,
		query.BindingID, authority.InputKind, authority.VectorSpace.ID, stored.SourceManifestChecksum,
		neighbors, query.Limit, query.Scope)
	if err != nil {
		return nil, coverage, false, err
	}
	if resolution.SourceManifestChecksum != stored.SourceManifestChecksum {
		return nil, coverage, false, store.ErrVectorIndexSourceStale
	}
	coverage.ScopedDocuments = resolution.ScopedDocuments
	coverage.CompleteDocuments = resolution.CompleteDocuments
	coverage.State = CoverageComplete
	if coverage.CompleteDocuments != coverage.ScopedDocuments {
		coverage.State = CoverageIncomplete
		if authority.BindingRequired && degradeIncompleteRequired {
			return nil, coverage, false, degradableSemanticFailure(DegradationIncompleteCoverage,
				errors.New("required semantic coverage became incomplete during retrieval"))
		}
	}
	resolved := resolution.Candidates
	if len(resolved) > query.Limit {
		resolved = resolved[:query.Limit]
		truncated = true
	}
	candidates := make([]Candidate, len(resolved))
	for index, item := range resolved {
		if item.VectorSpaceID != authority.VectorSpace.ID {
			return nil, coverage, false, errors.New("semantic result escaped the active vector space")
		}
		candidates[index] = Candidate{Document: DocumentIdentity{VaultID: item.VaultID,
			NodeID: item.NodeID, ContentVersionID: item.ContentVersionID}, Lane: LaneSemantic,
			Rank: index + 1, Score: item.Score, Path: item.Path, VectorSpaceID: item.VectorSpaceID,
			Evidence: []EvidenceReference{{Kind: "embedding", VaultID: item.VaultID,
				NodeID: item.NodeID, NodeRevision: item.NodeRevision, ContentVersionID: item.ContentVersionID,
				VectorSpaceID: item.VectorSpaceID, EmbeddingSetID: item.EmbeddingSetID,
				InputGenerationID: item.InputGenerationID, InputID: item.InputID,
				InputKind: item.InputKind, SourceManifestChecksum: resolution.SourceManifestChecksum}}}
	}
	return candidates, coverage, truncated || resolution.Truncated, nil
}

func normalizeQuery(query Query) (Query, error) {
	query.Text = strings.TrimSpace(query.Text)
	if query.Text == "" {
		return Query{}, errors.New("retrieval query text is required")
	}
	if query.Mode == "" {
		query.Mode = ModeAuto
	}
	if query.Mode != ModeAuto && query.Mode != ModeLexical && query.Mode != ModeSemantic && query.Mode != ModeHybrid {
		return Query{}, fmt.Errorf("unsupported retrieval mode %q", query.Mode)
	}
	if query.Limit == 0 {
		query.Limit = DefaultCandidateLimit
	}
	if query.Limit < 1 || query.Limit > MaxCandidateLimit {
		return Query{}, fmt.Errorf("retrieval candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	return query, nil
}

func (searcher *Searcher) collectLexical(ctx context.Context, query Query) ([]Candidate, bool, error) {
	hits, truncated, err := searcher.backend.SearchExplainedLexicalCandidates(ctx, query.Text, query.Limit, query.Scope)
	if err != nil {
		return nil, false, err
	}
	if len(hits) > query.Limit {
		hits = hits[:query.Limit]
		truncated = true
	}
	candidates := make([]Candidate, len(hits))
	for index, hit := range hits {
		candidates[index] = Candidate{Document: DocumentIdentity{VaultID: searcher.backend.VaultID(),
			NodeID: hit.Node.ID, ContentVersionID: hit.Node.CurrentVersionID}, Lane: LaneLexical,
			Rank: index + 1, Path: hit.Path, Excerpt: hit.Excerpt,
			Evidence: []EvidenceReference{{Kind: hit.EvidenceKind, VaultID: searcher.backend.VaultID(),
				NodeID: hit.Node.ID, NodeRevision: hit.Node.Revision, ContentVersionID: hit.Node.CurrentVersionID,
				BuildID: hit.BuildID, SegmentID: hit.SegmentID, BlobHash: hit.BlobHash}}}
	}
	return candidates, truncated, nil
}

func laneReport(requested, actual Mode, coverage Coverage, degradation Degradation,
	candidates []Candidate, truncated bool,
) Report {
	results := make([]Result, len(candidates))
	for index, candidate := range candidates {
		contribution := 1 / float64(ReciprocalRankK+candidate.Rank)
		result := Result{Document: candidate.Document, Rank: index + 1, Score: contribution,
			Path: candidate.Path, Excerpt: candidate.Excerpt, Evidence: candidate.Evidence,
			Explanation: []Contribution{{Lane: candidate.Lane, Rank: candidate.Rank,
				Contribution: contribution}}}
		if candidate.Lane == LaneLexical {
			result.LexicalRank = candidate.Rank
		} else {
			result.SemanticRank = candidate.Rank
		}
		results[index] = result
	}
	traceCode := TraceLexicalCandidates
	if actual == ModeSemantic {
		traceCode = TraceSemanticCandidates
	}
	return Report{RequestedMode: requested, ActualMode: actual, Coverage: coverage,
		Degradation: degradation, Results: results, Truncated: truncated,
		Trace: []TraceEvent{{Code: traceCode, Count: len(results)}}}
}

func makeHybridReport(requested Mode, coverage Coverage, lexical, semantic []Candidate,
	upstreamTruncated bool, limit int,
) (Report, error) {
	results, fusedTruncated, err := FuseReciprocalRank(lexical, semantic, limit)
	if err != nil {
		return Report{}, err
	}
	return Report{RequestedMode: requested, ActualMode: ModeHybrid, Coverage: coverage,
		Results: results, Truncated: upstreamTruncated || fusedTruncated,
		Trace: []TraceEvent{{Code: TraceLexicalCandidates, Count: len(lexical)},
			{Code: TraceSemanticCandidates, Count: len(semantic)},
			{Code: TraceFusedCandidates, Count: len(results)}}}, nil
}

func (searcher *Searcher) lexical(ctx context.Context, query Query, requested Mode,
	degradation Degradation, coverage Coverage,
) (Report, error) {
	candidates, truncated, err := searcher.collectLexical(ctx, query)
	if err != nil {
		return Report{}, err
	}
	return laneReport(requested, ModeLexical, coverage, degradation, candidates, truncated), nil
}
