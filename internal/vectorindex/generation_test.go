package vectorindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// This test fails if generation bytes depend on caller slice order or search
// stops using exact cosine ordering with stable logical-row tie breaks.
func TestBuildGenerationIsDeterministicAndSearchesExactCosine(t *testing.T) {
	sets := []document.VectorSetV1{
		testVectorSet(document.VectorMetricCosine, document.VectorNormalizationUnitLength,
			[]string{"chunk-b"}, [][]float32{{0, 1}}),
		testVectorSet(document.VectorMetricCosine, document.VectorNormalizationUnitLength,
			[]string{"chunk-a", "chunk-c"}, [][]float32{{1, 0}, {0, 1}}),
	}
	manifest := testManifest(t, sets)

	first, err := BuildGeneration(manifest, sets, Options{})
	require.NoError(t, err)
	second, err := BuildGeneration(manifest, []document.VectorSetV1{sets[1], sets[0]}, Options{})
	require.NoError(t, err)
	assert.Equal(t, first.Bytes(), second.Bytes())

	opened, err := OpenGeneration(bytes.NewReader(first.Bytes()), int64(len(first.Bytes())))
	require.NoError(t, err)
	neighbors, err := opened.Search([]float32{1, 0}, 3, 3)
	require.NoError(t, err)
	require.Len(t, neighbors, 3)
	assert.Equal(t, "chunk-a", neighbors[0].InputKey)
	assert.InDelta(t, 1.0, neighbors[0].Score, 1e-12)
	assert.Zero(t, neighbors[0].Distance)
	assert.Less(t, neighbors[1].SetID+"\x00"+neighbors[1].InputKey,
		neighbors[2].SetID+"\x00"+neighbors[2].InputKey)
	assert.Zero(t, neighbors[1].Score)
	assert.Zero(t, neighbors[2].Score)
}

// This test fails if a codec change alters any architecture-independent v1
// header, offset, identity, row-major float, or whole-generation checksum byte.
func TestGenerationEncodesGoldenBinary(t *testing.T) {
	sets := []document.VectorSetV1{
		testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
			[]string{"golden-a"}, [][]float32{{1.5, -2}}),
		testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
			[]string{"golden-b"}, [][]float32{{0, 3.25}}),
	}
	generation, err := BuildGeneration(testManifest(t, sets), sets, Options{})
	require.NoError(t, err)
	want, err := os.ReadFile("testdata/vector-index-v1.golden.bin")
	require.NoError(t, err)
	assert.Equal(t, want, generation.Bytes())
	digest := sha256.Sum256(want)
	assert.Equal(t, "20a953023d9e2c3a5793a09743531504fda8e922b84237bf57a5be15fc5415eb", hex.EncodeToString(digest[:]))
}

// This test fails if dot-product ranking stops preferring larger scores or L2
// ranking stops preferring smaller exact Euclidean distances.
func TestSearchUsesExactDotProductAndEuclideanOrdering(t *testing.T) {
	tests := []struct {
		name       string
		metric     string
		query      []float32
		vectors    [][]float32
		wantKeys   []string
		wantValues []float64
	}{
		{
			name: "dot product", metric: document.VectorMetricDotProduct,
			query: []float32{2, -1}, vectors: [][]float32{{1, 1}, {2, 0}, {-1, 0}},
			wantKeys: []string{"row-b", "row-a", "row-c"}, wantValues: []float64{4, 1, -2},
		},
		{
			name: "euclidean", metric: document.VectorMetricL2,
			query: []float32{1, 1}, vectors: [][]float32{{4, 5}, {1, 2}, {2, 3}},
			wantKeys: []string{"row-b", "row-c", "row-a"}, wantValues: []float64{1, math.Sqrt(5), 5},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := testVectorSet(test.metric, document.VectorNormalizationNone,
				[]string{"row-a", "row-b", "row-c"}, test.vectors)
			generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}),
				[]document.VectorSetV1{set}, Options{})
			require.NoError(t, err)
			neighbors, err := generation.Search(test.query, 3, 3)
			require.NoError(t, err)
			for index, neighbor := range neighbors {
				assert.Equal(t, test.wantKeys[index], neighbor.InputKey)
				if test.metric == document.VectorMetricL2 {
					assert.InDelta(t, test.wantValues[index], neighbor.Distance, 1e-12)
					assert.Zero(t, neighbor.Score)
				} else {
					assert.InDelta(t, test.wantValues[index], neighbor.Score, 1e-12)
					assert.Zero(t, neighbor.Distance)
				}
			}
		})
	}
}

func testVectorSet(metric, normalization string, keys []string, vectors [][]float32) document.VectorSetV1 {
	checksums := make([]string, len(keys))
	for index := range checksums {
		checksums[index] = strings.Repeat(string(rune('a'+index)), 64)
	}
	return document.VectorSetV1{
		VectorSpaceFingerprint: strings.Repeat("f", 64),
		Metric:                 metric, Normalization: normalization, Dimension: len(vectors[0]),
		InputKeys: keys, InputChecksums: checksums, Vectors: vectors,
	}
}

func testManifest(tb testing.TB, sets []document.VectorSetV1) Manifest {
	tb.Helper()
	setIDs := make([]string, len(sets))
	for index, set := range sets {
		_, checksum, err := document.EncodeVectorSetV1(set)
		require.NoError(tb, err)
		setIDs[index] = checksum
	}
	manifest, err := NewManifest(setIDs)
	require.NoError(tb, err)
	return manifest
}
