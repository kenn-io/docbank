package rrf_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/rrf"
)

func TestFuseScoresRanksAndStableTies(t *testing.T) {
	const k = 60.0
	hits, err := rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "lexical", Weight: 1, Keys: []string{"a", "b", "a"}},
		{Name: "vector", Weight: 2, Keys: []string{"b", "c"}},
	})
	require.NoError(t, err)
	require.Len(t, hits, 3)

	byKey := map[string]rrf.Hit[string]{}
	for _, hit := range hits {
		byKey[hit.Key] = hit
	}
	assert.InDelta(t, 1/(k+1), byKey["a"].Score, 1e-12)
	assert.Len(t, byKey["a"].Contributions, 1)
	assert.Equal(t, 1, byKey["a"].Contributions[0].Rank)

	assert.InDelta(t, 1/(k+2)+2/(k+1), byKey["b"].Score, 1e-12)
	require.Len(t, byKey["b"].Contributions, 2)
	assert.Equal(t, "lexical", byKey["b"].Contributions[0].Leg)
	assert.Equal(t, 2, byKey["b"].Contributions[0].Rank)
	assert.Equal(t, "vector", byKey["b"].Contributions[1].Leg)
	assert.Equal(t, 1, byKey["b"].Contributions[1].Rank)

	assert.Equal(t, []string{"b", "c", "a"}, []string{hits[0].Key, hits[1].Key, hits[2].Key})

	tied, err := rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "left", Weight: 1, Keys: []string{"a"}},
		{Name: "right", Weight: 1, Keys: []string{"b"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, []string{tied[0].Key, tied[1].Key})
	swapped, err := rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "right", Weight: 1, Keys: []string{"b"}},
		{Name: "left", Weight: 1, Keys: []string{"a"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, []string{swapped[0].Key, swapped[1].Key})

	_, err = rrf.Fuse(0, []rrf.Leg[string]{{Name: "lexical", Weight: 1, Keys: []string{"a"}}})
	require.Error(t, err)
	_, err = rrf.Fuse(k, []rrf.Leg[string]{{Name: "lexical", Weight: 0, Keys: []string{"a"}}})
	require.Error(t, err)
	_, err = rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "lexical", Weight: 1},
		{Name: "lexical", Weight: 1},
	})
	require.Error(t, err)
}

func TestFuseGroupsKeepsAlternatesUntilTheCallerFilters(t *testing.T) {
	hits, err := rrf.FuseGroups(60, []rrf.GroupLeg[string, string]{
		{Name: "lexical", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "g1", Members: []string{"fresh", "stale"}},
			{Key: "g2", Members: []string{"only-lexical"}},
			{Key: "g1", Members: []string{"extra"}},
		}},
		{Name: "vector", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "g1", Members: []string{"stale", "vector-only"}},
		}},
	})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "g1", hits[0].Group)
	require.Len(t, hits[0].Contributions, 2, "a repeated group in one leg contributes once")
	assert.Equal(t, 1, hits[0].Contributions[0].Rank)
	assert.Equal(t, []rrf.Alternate[string]{
		{Member: "fresh", Leg: "lexical"},
		{Member: "stale", Leg: "lexical"},
		{Member: "extra", Leg: "lexical"},
		{Member: "stale", Leg: "vector"},
		{Member: "vector-only", Leg: "vector"},
	}, hits[0].Alternates)
	assert.Equal(t, "g2", hits[1].Group)
	assert.Len(t, hits[1].Contributions, 1)
}

func TestFuseEveryKeepsOnlyGroupsEveryLegFound(t *testing.T) {
	// s1 has evidence for both concepts in different members. s2 repeats in
	// the first leg but never appears in the second, so it is dropped.
	legs := []rrf.GroupLeg[string, string]{
		{Name: "auth", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "s2", Members: []string{"m1"}},
			{Key: "s1", Members: []string{"m4", "m9"}},
			{Key: "s3", Members: []string{"m2"}},
			{Key: "s2", Members: []string{"m7"}},
		}},
		{Name: "retry", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "s3", Members: []string{"m8"}},
			{Key: "s1", Members: []string{"m12"}},
		}},
	}
	hits, err := rrf.FuseGroupsEvery(60, legs)
	require.NoError(t, err)
	require.Len(t, hits, 2)

	// s3 ranks 3 and 1, s1 ranks 2 and 2. With k = 60, 1/63 + 1/61 is
	// slightly larger than 2/62, so s3 leads.
	assert.Equal(t, "s3", hits[0].Group)
	assert.Equal(t, "s1", hits[1].Group)
	for _, hit := range hits {
		assert.Len(t, hit.Contributions, 2)
	}
	s1 := hits[1]
	assert.Equal(t, []rrf.Alternate[string]{
		{Member: "m4", Leg: "auth"},
		{Member: "m9", Leg: "auth"},
		{Member: "m12", Leg: "retry"},
	}, s1.Alternates)
}

func TestFuseEveryWithOneLegMatchesFuseGroups(t *testing.T) {
	legs := []rrf.GroupLeg[string, string]{
		{Name: "only", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "a", Members: []string{"x"}},
			{Key: "b", Members: []string{"y"}},
		}},
	}
	every, err := rrf.FuseGroupsEvery(60, legs)
	require.NoError(t, err)
	groups, err := rrf.FuseGroups(60, legs)
	require.NoError(t, err)
	assert.Equal(t, groups, every)

	_, err = rrf.FuseGroupsEvery(60, []rrf.GroupLeg[string, string]{
		{Name: "dup", Weight: 1}, {Name: "dup", Weight: 1},
	})
	require.Error(t, err)
}

func TestFuseRejectsAScoreThatOverflows(t *testing.T) {
	legs := make([]rrf.Leg[string], 62)
	for i := range legs {
		legs[i] = rrf.Leg[string]{
			Name: fmt.Sprintf("leg-%d", i), Weight: math.MaxFloat64, Keys: []string{"doc"},
		}
	}
	hits, err := rrf.Fuse(60, legs)
	require.ErrorContains(t, err, "non-finite")
	assert.Empty(t, hits)
}

func TestFuseGroupsRejectsAMemberThatCannotBeCompared(t *testing.T) {
	members, err := rrf.FuseGroups(60, []rrf.GroupLeg[string, float64]{{
		Name: "lexical", Weight: 1,
		Groups: []rrf.Group[string, float64]{{Key: "g", Members: []float64{1, math.NaN()}}},
	}})
	require.ErrorContains(t, err, "NaN")
	assert.Empty(t, members)

	// A driver can scan text into an interface as []byte, which panics when
	// compared.
	members2, err := rrf.FuseGroups(60, []rrf.GroupLeg[string, any]{{
		Name: "lexical", Weight: 1,
		Groups: []rrf.Group[string, any]{{Key: "g", Members: []any{[]byte("body")}}},
	}})
	require.ErrorContains(t, err, "not comparable")
	assert.Empty(t, members2)
}

func TestFuseRejectsNonFiniteKAndWeight(t *testing.T) {
	tests := []struct {
		name   string
		k      float64
		weight float64
	}{
		{name: "nan k", k: math.NaN(), weight: 1},
		{name: "+inf k", k: math.Inf(1), weight: 1},
		{name: "nan weight", k: 60, weight: math.NaN()},
		{name: "+inf weight", k: 60, weight: math.Inf(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rrf.Fuse(tt.k, []rrf.Leg[string]{{
				Name: "lexical", Weight: tt.weight, Keys: []string{"a"},
			}})
			require.Error(t, err)

			_, err = rrf.FuseGroups(tt.k, []rrf.GroupLeg[string, string]{{
				Name:   "lexical",
				Weight: tt.weight,
				Groups: []rrf.Group[string, string]{{Key: "g"}},
			}})
			require.Error(t, err)
		})
	}

	hits, err := rrf.Fuse(60, []rrf.Leg[string]{
		{Name: "lexical", Weight: 1, Keys: []string{"a", "b"}},
		{Name: "vector", Weight: 2, Keys: []string{"b"}},
	})
	require.NoError(t, err)
	var got float64
	for _, hit := range hits {
		if hit.Key == "b" {
			got = hit.Score
		}
	}
	assert.InDelta(t, 1.0/(60+2)+2.0/(60+1), got, 1e-12)
}
