package embedding_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/embedding"
)

func TestSearchDefaultsToLexicalWithSharedCandidateLimit(t *testing.T) {
	options, err := embedding.NormalizeSearchOptions(embedding.SearchOptions{})
	require.NoError(t, err)
	assert.Equal(t, embedding.SearchModeLexical, options.Mode)
	assert.Equal(t, embedding.DefaultCandidateLimit, options.CandidateLimit)

	options, err = embedding.NormalizeSearchOptions(embedding.SearchOptions{
		Mode: embedding.SearchModeSemantic, CandidateLimit: 37,
	})
	require.NoError(t, err)
	assert.Equal(t, 37, options.CandidateLimit)
}

func TestCollectScopedCandidatesUsesOverflowProbe(t *testing.T) {
	source := &slicePageSource{candidates: []embedding.Candidate{
		{Key: "in-scope-1", Score: 0.9}, {Key: "in-scope-2", Score: 0.8}, {Key: "in-scope-3", Score: 0.7},
	}}
	result, err := embedding.CollectScopedCandidates(context.Background(), source, 2, 1)
	require.NoError(t, err)
	assert.True(t, result.Truncated)
	assert.Equal(t, []embedding.RankedCandidate{
		{Key: "in-scope-1", Rank: 1, Score: 0.9}, {Key: "in-scope-2", Rank: 2, Score: 0.8},
	}, result.Candidates)
	assert.Equal(t, 3, source.calls)

	exact := &slicePageSource{candidates: source.candidates[:2]}
	result, err = embedding.CollectScopedCandidates(context.Background(), exact, 2, 1)
	require.NoError(t, err)
	assert.False(t, result.Truncated)
	assert.Len(t, result.Candidates, 2)
}

func TestRetrievalPolicyAppliesIndependentLaneLimits(t *testing.T) {
	policy, err := embedding.NewRetrievalPolicy(2, 1)
	require.NoError(t, err)
	candidates := []embedding.Candidate{
		{Key: "one", Score: 0.9}, {Key: "two", Score: 0.8}, {Key: "three", Score: 0.7},
	}

	lexical, err := policy.CollectLexical(context.Background(), &slicePageSource{candidates: candidates}, 1)
	require.NoError(t, err)
	assert.Len(t, lexical.Candidates, 2)
	assert.True(t, lexical.Truncated)

	semantic, err := policy.CollectSemantic(context.Background(), &slicePageSource{candidates: candidates}, 1)
	require.NoError(t, err)
	assert.Len(t, semantic.Candidates, 1)
	assert.True(t, semantic.Truncated)
}

func TestHybridFusionPreservesSignalsAndDetectsUnionOverflow(t *testing.T) {
	result, err := embedding.FuseReciprocalRank(embedding.FusionInput{
		Lexical: embedding.ScopedCandidates{Candidates: []embedding.RankedCandidate{
			{Key: "shared", Rank: 1, Score: 11, Provenance: []embedding.Provenance{{Kind: "person", Value: "synthetic-person"}}},
			{Key: "lexical", Rank: 2, Score: 7},
		}},
		Semantic: embedding.ScopedCandidates{Candidates: []embedding.RankedCandidate{
			{Key: "shared", Rank: 1, Score: 0.91}, {Key: "semantic", Rank: 2, Score: 0.83},
		}},
	}, 2)
	require.NoError(t, err)
	require.Len(t, result.Candidates, 2)
	assert.True(t, result.Truncated)
	assert.Equal(t, "shared", result.Candidates[0].Key)
	assert.NotNil(t, result.Candidates[0].Lexical)
	assert.NotNil(t, result.Candidates[0].Semantic)
	assert.Equal(t, []embedding.Provenance{{Kind: "person", Value: "synthetic-person"}}, result.Candidates[0].Lexical.Provenance)
}

type slicePageSource struct {
	candidates []embedding.Candidate
	calls      int
}

func (source *slicePageSource) SearchPage(_ context.Context, request embedding.PageRequest) (embedding.CandidatePage, error) {
	source.calls++
	start := 0
	if request.Cursor != "" {
		parsed, err := strconv.Atoi(request.Cursor)
		if err != nil {
			return embedding.CandidatePage{}, fmt.Errorf("parse test cursor: %w", err)
		}
		start = parsed
	}
	end := min(start+request.Limit, len(source.candidates))
	return embedding.CandidatePage{
		Candidates: source.candidates[start:end], NextCursor: strconv.Itoa(end), Exhausted: end == len(source.candidates),
	}, nil
}
