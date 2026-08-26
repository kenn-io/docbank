package embedding

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document"
)

const (
	// DefaultCandidateLimit is the shared lexical, semantic, and hybrid default.
	DefaultCandidateLimit = 100
	// MaxCandidateLimit bounds final scoped candidate collection.
	MaxCandidateLimit = document.MaxRetrievalCandidateLimit
	// DefaultReciprocalRankConstant is the shared RRF smoothing constant.
	DefaultReciprocalRankConstant = 60
)

// SearchMode selects query behavior. Auto is deliberately lexical so omitted
// mode never sends query plaintext to an embedding provider.
type SearchMode string

const (
	SearchModeAuto     SearchMode = "auto"
	SearchModeLexical  SearchMode = "lexical"
	SearchModeSemantic SearchMode = "semantic"
	SearchModeHybrid   SearchMode = "hybrid"
)

// SearchOptions contains shared search controls.
type SearchOptions struct {
	Mode           SearchMode
	CandidateLimit int
}

// RetrievalPolicy binds the independent lexical and semantic candidate
// ceilings selected by deployment configuration.
type RetrievalPolicy struct {
	lexicalLimit int
	vectorLimit  int
}

// NewRetrievalPolicy validates and seals independent retrieval-lane limits.
func NewRetrievalPolicy(lexicalLimit, vectorLimit int) (RetrievalPolicy, error) {
	if lexicalLimit < 1 || lexicalLimit > MaxCandidateLimit {
		return RetrievalPolicy{}, fmt.Errorf("lexical candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	if vectorLimit < 1 || vectorLimit > MaxCandidateLimit {
		return RetrievalPolicy{}, fmt.Errorf("vector candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	return RetrievalPolicy{lexicalLimit: lexicalLimit, vectorLimit: vectorLimit}, nil
}

// LexicalLimit returns the configured lexical-lane candidate ceiling.
func (p RetrievalPolicy) LexicalLimit() int { return p.lexicalLimit }

// VectorLimit returns the configured semantic vector-lane candidate ceiling.
func (p RetrievalPolicy) VectorLimit() int { return p.vectorLimit }

// CollectLexical applies the configured lexical ceiling to scoped collection.
func (p RetrievalPolicy) CollectLexical(
	ctx context.Context, source ScopedPageSource, pageSize int,
) (ScopedCandidates, error) {
	return CollectScopedCandidates(ctx, source, p.lexicalLimit, pageSize)
}

// CollectSemantic applies the configured vector ceiling to scoped collection.
func (p RetrievalPolicy) CollectSemantic(
	ctx context.Context, source ScopedPageSource, pageSize int,
) (ScopedCandidates, error) {
	return CollectScopedCandidates(ctx, source, p.vectorLimit, pageSize)
}

// NormalizeSearchOptions applies shared defaults and validates public bounds.
func NormalizeSearchOptions(options SearchOptions) (SearchOptions, error) {
	if options.Mode == "" || options.Mode == SearchModeAuto {
		options.Mode = SearchModeLexical
	}
	if options.Mode != SearchModeLexical && options.Mode != SearchModeSemantic && options.Mode != SearchModeHybrid {
		return SearchOptions{}, fmt.Errorf("unsupported document search mode %q", options.Mode)
	}
	if options.CandidateLimit == 0 {
		options.CandidateLimit = DefaultCandidateLimit
	}
	if options.CandidateLimit < 1 || options.CandidateLimit > MaxCandidateLimit {
		return SearchOptions{}, fmt.Errorf("document search candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	return options, nil
}

// PageRequest requests one deterministic page of already-scoped candidates.
// Scope must be applied by the source before its global vector cutoff.
type PageRequest struct {
	Cursor string
	Limit  int
}

// Provenance is an opaque application-defined fact that must survive ranking
// and hybrid fusion, such as a matching person or source occurrence.
type Provenance struct {
	Kind  string
	Value string
}

// Candidate is one source-ranked search candidate.
type Candidate struct {
	Key        string
	Score      float64
	Provenance []Provenance
}

// CandidatePage is one stable page. Exhausted is authoritative; callers must
// not infer exhaustion merely because a short page was returned.
type CandidatePage struct {
	Candidates []Candidate
	NextCursor string
	Exhausted  bool
}

// ScopedPageSource returns candidates after applying every requested scope
// constraint. Implementations must provide stable ordering and cursors.
type ScopedPageSource interface {
	SearchPage(ctx context.Context, request PageRequest) (CandidatePage, error)
}

// RankedCandidate records a candidate's rank in one retrieval lane.
type RankedCandidate struct {
	Key        string
	Rank       int
	Score      float64
	Provenance []Provenance
}

// ScopedCandidates is a bounded, scope-complete candidate set.
type ScopedCandidates struct {
	Candidates []RankedCandidate
	Truncated  bool
}

// CollectScopedCandidates pages an already-scoped backend until it observes
// candidateLimit+1 unique candidates or authoritative exhaustion. pageSize is
// an independent backend bound and may be smaller than candidateLimit.
func CollectScopedCandidates(ctx context.Context, source ScopedPageSource, candidateLimit, pageSize int) (ScopedCandidates, error) {
	if source == nil {
		return ScopedCandidates{}, errors.New("scoped candidate source is required")
	}
	if candidateLimit < 1 || candidateLimit > MaxCandidateLimit {
		return ScopedCandidates{}, fmt.Errorf("candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	if pageSize < 1 || pageSize > MaxCandidateLimit {
		return ScopedCandidates{}, fmt.Errorf("candidate page size must be between 1 and %d", MaxCandidateLimit)
	}

	wanted := candidateLimit + 1
	cursor := ""
	seen := make(map[string]bool, wanted)
	collected := make([]RankedCandidate, 0, wanted)
	for len(collected) < wanted {
		requestLimit := min(pageSize, wanted-len(collected))
		page, err := source.SearchPage(ctx, PageRequest{Cursor: cursor, Limit: requestLimit})
		if err != nil {
			return ScopedCandidates{}, err
		}
		if len(page.Candidates) > requestLimit {
			return ScopedCandidates{}, errors.New("scoped candidate source exceeded requested page size")
		}
		if len(page.Candidates) == 0 && !page.Exhausted {
			return ScopedCandidates{}, errors.New("non-exhausted candidate page returned no candidates")
		}
		for _, candidate := range page.Candidates {
			if candidate.Key == "" {
				return ScopedCandidates{}, errors.New("scoped candidate key is empty")
			}
			if seen[candidate.Key] {
				return ScopedCandidates{}, fmt.Errorf("scoped candidate %q was returned more than once", candidate.Key)
			}
			seen[candidate.Key] = true
			collected = append(collected, RankedCandidate{
				Key: candidate.Key, Rank: len(collected) + 1, Score: candidate.Score,
				Provenance: slices.Clone(candidate.Provenance),
			})
		}
		if page.Exhausted {
			break
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return ScopedCandidates{}, errors.New("non-exhausted candidate page did not advance its cursor")
		}
		cursor = page.NextCursor
	}

	result := ScopedCandidates{Candidates: collected, Truncated: len(collected) > candidateLimit}
	if result.Truncated {
		result.Candidates = result.Candidates[:candidateLimit]
	}
	return result, nil
}

// CandidateSignal preserves one lane's score and rank through hybrid fusion.
type CandidateSignal struct {
	Rank       int
	Score      float64
	Provenance []Provenance
}

// FusedCandidate is one deterministic reciprocal-rank-fused result.
type FusedCandidate struct {
	Key      string
	Rank     int
	Score    float64
	Lexical  *CandidateSignal
	Semantic *CandidateSignal
}

// FusionInput contains the fully scoped results from each retrieval lane.
type FusionInput struct {
	Lexical  ScopedCandidates
	Semantic ScopedCandidates
}

// FusedCandidates contains bounded hybrid results and accurate upstream or
// post-fusion overflow metadata.
type FusedCandidates struct {
	Candidates []FusedCandidate
	Truncated  bool
}

// FuseReciprocalRank combines lexical and semantic candidates with
// deterministic RRF ordering while preserving both source signals.
func FuseReciprocalRank(input FusionInput, candidateLimit int) (FusedCandidates, error) {
	if candidateLimit < 1 || candidateLimit > MaxCandidateLimit {
		return FusedCandidates{}, fmt.Errorf("candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	byKey := make(map[string]*FusedCandidate, len(input.Lexical.Candidates)+len(input.Semantic.Candidates))
	if err := addFusionLane(byKey, input.Lexical.Candidates, true); err != nil {
		return FusedCandidates{}, fmt.Errorf("lexical candidates: %w", err)
	}
	if err := addFusionLane(byKey, input.Semantic.Candidates, false); err != nil {
		return FusedCandidates{}, fmt.Errorf("semantic candidates: %w", err)
	}
	candidates := make([]FusedCandidate, 0, len(byKey))
	for _, candidate := range byKey {
		candidates = append(candidates, *candidate)
	}
	slices.SortFunc(candidates, func(left, right FusedCandidate) int {
		switch {
		case left.Score > right.Score:
			return -1
		case left.Score < right.Score:
			return 1
		case left.Key < right.Key:
			return -1
		case left.Key > right.Key:
			return 1
		default:
			return 0
		}
	})
	for index := range candidates {
		candidates[index].Rank = index + 1
	}
	truncated := input.Lexical.Truncated || input.Semantic.Truncated || len(candidates) > candidateLimit
	if len(candidates) > candidateLimit {
		candidates = candidates[:candidateLimit]
	}
	return FusedCandidates{Candidates: candidates, Truncated: truncated}, nil
}

func addFusionLane(byKey map[string]*FusedCandidate, candidates []RankedCandidate, lexical bool) error {
	seen := make(map[string]bool, len(candidates))
	lastRank := 0
	for _, source := range candidates {
		if source.Key == "" || source.Rank <= lastRank || seen[source.Key] {
			return errors.New("candidate keys must be unique with strictly increasing positive ranks")
		}
		seen[source.Key] = true
		lastRank = source.Rank
		candidate := byKey[source.Key]
		if candidate == nil {
			candidate = &FusedCandidate{Key: source.Key}
			byKey[source.Key] = candidate
		}
		signal := &CandidateSignal{
			Rank: source.Rank, Score: source.Score, Provenance: slices.Clone(source.Provenance),
		}
		candidate.Score += 1 / float64(DefaultReciprocalRankConstant+source.Rank)
		if lexical {
			candidate.Lexical = signal
		} else {
			candidate.Semantic = signal
		}
	}
	return nil
}
