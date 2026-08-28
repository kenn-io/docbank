package vectorindex_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// This test fails if a caller reopening persisted bytes cannot compare the
// validated projection to current authority or discover the exact visit bound.
func TestOpenedGenerationExposesCopySafeValidatedMetadata(t *testing.T) {
	set := document.VectorSetV1{
		VectorSpaceFingerprint: strings.Repeat("f", 64),
		Metric:                 document.VectorMetricDotProduct,
		Normalization:          document.VectorNormalizationNone,
		Dimension:              2,
		InputKeys:              []string{"row-a", "row-b"},
		InputChecksums:         []string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		Vectors:                [][]float32{{1, 0}, {0, 1}},
	}
	_, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{setID})
	require.NoError(t, err)
	built, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	opened, err := vectorindex.OpenGeneration(bytes.NewReader(built.Bytes()), int64(len(built.Bytes())))
	require.NoError(t, err)

	metadata := opened.Metadata()
	assert.Equal(t, "docbank-vector-index/v1", metadata.Format)
	assert.Equal(t, "exact-row-major-f32le/v1", metadata.Layout)
	assert.Equal(t, manifest.Checksum, metadata.Manifest.Checksum)
	assert.Equal(t, manifest.SetIDs, metadata.Manifest.SetIDs)
	assert.Equal(t, set.VectorSpaceFingerprint, metadata.VectorSpaceID)
	assert.Equal(t, set.Metric, metadata.Metric)
	assert.Equal(t, set.Normalization, metadata.Normalization)
	assert.Equal(t, set.Dimension, metadata.Dimension)
	assert.Equal(t, len(set.Vectors), metadata.RowCount)

	metadata.Manifest.SetIDs[0] = strings.Repeat("0", 64)
	assert.Equal(t, setID, opened.Metadata().Manifest.SetIDs[0])
	neighbors, err := opened.Search([]float32{1, 0}, 1, opened.Metadata().RowCount)
	require.NoError(t, err)
	assert.Equal(t, "row-a", neighbors[0].InputKey)
}
