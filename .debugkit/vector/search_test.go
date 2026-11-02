package vector_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

func docs[K comparable](hits []vector.Hit[K]) []K {
	out := make([]K, len(hits))
	for i, h := range hits {
		out[i] = h.Doc
	}
	return out
}

func TestRollupByDocumentKeepsBestChunkPerDoc(t *testing.T) {
	assert := assert.New(t)
	hits := []vector.Hit[int64]{
		{Doc: 1, ChunkIndex: 0, Score: 0.2},
		{Doc: 2, ChunkIndex: 0, Score: 0.9},
		{Doc: 1, ChunkIndex: 3, Score: 0.7}, // better chunk for doc 1
		{Doc: 2, ChunkIndex: 1, Score: 0.4},
	}

	got, err := vector.RollupByDocument(hits)
	require.NoError(t, err)

	assert.Equal([]int64{2, 1}, docs(got), "one hit per doc, ordered by score desc")
	assert.Equal(3, got[1].ChunkIndex, "doc 1 keeps its highest-scoring chunk")
	assert.InDelta(0.7, got[1].Score, 1e-6)
}

func TestMergeUnionsAndPrefersEarlierGeneration(t *testing.T) {
	assert := assert.New(t)
	// String keys stand in for kata's UUIDs; building generation first.
	building := []vector.Hit[string]{
		{Doc: "shared", Score: 0.50},
		{Doc: "new-only", Score: 0.40},
	}
	active := []vector.Hit[string]{
		{Doc: "shared", Score: 0.99}, // higher raw score, but less preferred
		{Doc: "old-only", Score: 0.80},
	}

	got, err := vector.Merge([][]vector.Hit[string]{building, active}, vector.MergeOptions{Strategy: vector.MergeRawScore})
	require.NoError(t, err)

	assert.ElementsMatch([]string{"shared", "new-only", "old-only"}, docs(got), "coverage is a union")
	for _, h := range got {
		if h.Doc == "shared" {
			assert.InDelta(0.50, h.Score, 1e-6, "shared doc keeps the preferred (building) hit, not the higher raw score")
		}
	}
}

func TestMergeNormalizedScoreIsDefault(t *testing.T) {
	assert := assert.New(t)
	// Active generation scores live in a compressed high band; building
	// generation in a low band. Raw merge would let active dominate;
	// normalization puts each generation's top hit at 1.0.
	active := []vector.Hit[int]{
		{Doc: 1, Score: 0.90},
		{Doc: 2, Score: 0.85},
	}
	building := []vector.Hit[int]{
		{Doc: 3, Score: 0.20},
		{Doc: 4, Score: 0.10},
	}

	got, err := vector.Merge([][]vector.Hit[int]{building, active}, vector.MergeOptions{})
	require.NoError(t, err)

	// Each generation's best-normalized hit should reach the top band.
	top := got[0]
	assert.Contains([]int{1, 3}, top.Doc, "a normalized top hit leads, not just the raw-highest")
	assert.InDelta(1.0, float64(top.Score), 1e-6)
}

func TestMergeReciprocalRankFusesAcrossGenerations(t *testing.T) {
	assert := assert.New(t)
	// "shared" is rank 1 in one list and rank 2 in the other, so its
	// fused score should beat docs that appear in only one list.
	a := []vector.Hit[int]{
		{Doc: 10, Score: 0.99},
		{Doc: 99, Score: 0.98},
	}
	b := []vector.Hit[int]{
		{Doc: 99, Score: 0.50},
		{Doc: 20, Score: 0.49},
	}

	got, err := vector.Merge([][]vector.Hit[int]{a, b}, vector.MergeOptions{Strategy: vector.MergeReciprocalRank})
	require.NoError(t, err)

	assert.Equal(99, got[0].Doc, "the doc found in both generations ranks first")
}

func TestMergeRespectsLimit(t *testing.T) {
	assert := assert.New(t)
	list := []vector.Hit[int]{{Doc: 1, Score: 0.9}, {Doc: 2, Score: 0.8}, {Doc: 3, Score: 0.7}}

	got, err := vector.Merge([][]vector.Hit[int]{list}, vector.MergeOptions{Strategy: vector.MergeRawScore, Limit: 2})
	require.NoError(t, err)

	assert.Len(got, 2)
	assert.Equal([]int{1, 2}, docs(got))
}

func TestMergeEmpty(t *testing.T) {
	got, err := vector.Merge[int](nil, vector.MergeOptions{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMergeNormalizedScoreKeepsExtremeFiniteScoresFinite(t *testing.T) {
	got, err := vector.Merge([][]vector.Hit[int]{{
		{Doc: 1, Score: math.MaxFloat32},
		{Doc: 2, Score: -math.MaxFloat32},
	}}, vector.MergeOptions{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].Doc)
	assert.InDelta(t, 1, float64(got[0].Score), 1e-5)
	assert.Equal(t, 2, got[1].Doc)
	assert.InDelta(t, 0, float64(got[1].Score), 1e-5)
	for _, hit := range got {
		assert.False(t, math.IsNaN(float64(hit.Score)))
	}
}

func TestRollupAndMergeRejectNaN(t *testing.T) {
	got, err := vector.RollupByDocument([]vector.Hit[int64]{{Doc: 1, Score: float32(math.NaN())}})
	require.ErrorIs(t, err, vector.ErrNonFinite)
	assert.Empty(t, got)

	byDoc, err := vector.RollupByDocument([]vector.Hit[float64]{{Doc: math.NaN(), Score: 1}})
	require.ErrorIs(t, err, vector.ErrNonFinite)
	assert.Empty(t, byDoc)

	merged, err := vector.Merge([][]vector.Hit[int]{{{Doc: 1, Score: float32(math.NaN())}}}, vector.MergeOptions{Strategy: vector.MergeRawScore})
	require.ErrorIs(t, err, vector.ErrNonFinite)
	assert.Empty(t, merged)

	infinite, err := vector.Merge([][]vector.Hit[int]{{{Doc: 1, Score: float32(math.Inf(1))}}}, vector.MergeOptions{Strategy: vector.MergeRawScore})
	require.ErrorIs(t, err, vector.ErrNonFinite)
	assert.Empty(t, infinite)

	for _, constant := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err = vector.Merge([][]vector.Hit[int]{{{Doc: 1, Score: 1}}}, vector.MergeOptions{
			Strategy:     vector.MergeReciprocalRank,
			RankConstant: constant,
		})
		require.ErrorIs(t, err, vector.ErrNonFinite)
	}
	defaulted, err := vector.Merge([][]vector.Hit[int]{{{Doc: 1, Score: 1}}}, vector.MergeOptions{
		Strategy:     vector.MergeReciprocalRank,
		RankConstant: 0,
	})
	require.NoError(t, err)
	require.Len(t, defaulted, 1)
}
