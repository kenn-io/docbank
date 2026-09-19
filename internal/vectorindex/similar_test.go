package vectorindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestSearchSimilarRowsAllMetricsAndTies(t *testing.T) {
	for _, metric := range []string{document.VectorMetricCosine, document.VectorMetricDotProduct, document.VectorMetricL2} {
		t.Run(metric, func(t *testing.T) {
			set := testVectorSet(metric, document.VectorNormalizationNone, []string{"source-a", "source-c", "source-b", "candidate-a", "candidate-b", "far"}, [][]float32{{1, 0}, {0, 2}, {0, 2}, {0, 2}, {0, 2}, {-1, 0}})
			generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}), []document.VectorSetV1{set}, Options{})
			require.NoError(t, err)
			rows := generation.rows
			got, err := generation.SearchSimilarRows(t.Context(), rows[:3], rows[3:])
			require.NoError(t, err)
			require.Len(t, got, 3)
			assert.Equal(t, "candidate-a", got[0].InputKey)
			assert.Equal(t, "candidate-b", got[1].InputKey)
			assert.Equal(t, "source-b", got[0].SourceRow.InputKey)
			switch metric {
			case document.VectorMetricCosine:
				assert.InDelta(t, 1, got[0].Score, 1e-12)
			case document.VectorMetricDotProduct:
				assert.InDelta(t, 4.0, got[0].Score, 1e-12)
			case document.VectorMetricL2:
				assert.Zero(t, got[0].Distance)
				assert.Positive(t, got[2].Distance)
			}
			reversed, err := generation.SearchSimilarRows(t.Context(), []RowIdentity{rows[2], rows[1], rows[0]}, []RowIdentity{rows[5], rows[4], rows[3]})
			require.NoError(t, err)
			assert.Equal(t, got, reversed)
		})
	}
}

func TestSearchSimilarRowsRejectsInvalidIdentityAndAcceptsEmptyCandidates(t *testing.T) {
	set := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone, []string{"source"}, [][]float32{{1, 0}})
	generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}), []document.VectorSetV1{set}, Options{})
	require.NoError(t, err)
	for _, sources := range [][]RowIdentity{nil, {generation.rows[0], generation.rows[0]}, {{InputKey: "absent"}}} {
		_, err := generation.SearchSimilarRows(t.Context(), sources, nil)
		require.Error(t, err)
	}
	for _, candidates := range [][]RowIdentity{{generation.rows[0], generation.rows[0]}, {{InputKey: "absent"}}} {
		_, err := generation.SearchSimilarRows(t.Context(), generation.rows, candidates)
		require.Error(t, err)
	}
	_, err = (*Generation)(nil).SearchSimilarRows(t.Context(), generation.rows, nil)
	require.Error(t, err)
	got, err := generation.SearchSimilarRows(t.Context(), generation.rows, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}
