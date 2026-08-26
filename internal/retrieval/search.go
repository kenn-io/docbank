package retrieval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
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

type SearcherConfig struct {
	Backend       Backend
	Encoders      QueryEncoderResolver
	Owner         string
	LeaseDuration time.Duration
	Clock         func() time.Time
}

type Searcher struct {
	backend       Backend
	encoders      QueryEncoderResolver
	owner         string
	leaseDuration time.Duration
	clock         func() time.Time
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
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Searcher{backend: config.Backend, encoders: config.Encoders, owner: config.Owner,
		leaseDuration: config.LeaseDuration, clock: config.Clock}, nil
}

func (searcher *Searcher) Search(ctx context.Context, query Query) (Report, error) {
	query, err := normalizeQuery(query)
	if err != nil {
		return Report{}, err
	}
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
	if err := document.ValidateEmbeddingProviderRequest(provider, inputs, query.Authorization); err != nil {
		return nil, coverage, false, err
	}
	embedded, err := document.ExecuteEmbedding(ctx, provider, inputs, query.Authorization)
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
	neighbors, err := generation.Search(embedded.Vectors[0].Values, metadata.RowCount, metadata.RowCount)
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
	candidates := make([]Candidate, len(resolved))
	for index, item := range resolved {
		if item.VectorSpaceID != authority.VectorSpace.ID {
			return nil, coverage, false, errors.New("semantic result escaped the active vector space")
		}
		candidates[index] = Candidate{Document: DocumentIdentity{VaultID: item.VaultID,
			NodeID: item.NodeID, ContentVersionID: item.ContentVersionID}, Lane: LaneSemantic,
			Rank: index + 1, Score: item.Score, Path: item.Path, VectorSpaceID: item.VectorSpaceID,
			Evidence: []EvidenceReference{{Kind: "embedding", VaultID: item.VaultID,
				NodeID: item.NodeID, ContentVersionID: item.ContentVersionID,
				VectorSpaceID: item.VectorSpaceID, EmbeddingSetID: item.EmbeddingSetID,
				InputGenerationID: item.InputGenerationID, InputID: item.InputID,
				InputKind: item.InputKind}}}
	}
	return candidates, coverage, resolution.Truncated, nil
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
	candidates := make([]Candidate, len(hits))
	for index, hit := range hits {
		candidates[index] = Candidate{Document: DocumentIdentity{VaultID: searcher.backend.VaultID(),
			NodeID: hit.Node.ID, ContentVersionID: hit.Node.CurrentVersionID}, Lane: LaneLexical,
			Rank: index + 1, Path: hit.Path, Excerpt: hit.Excerpt,
			Evidence: []EvidenceReference{{Kind: hit.EvidenceKind, VaultID: searcher.backend.VaultID(),
				NodeID: hit.Node.ID, ContentVersionID: hit.Node.CurrentVersionID,
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
