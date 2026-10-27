package vectorindex_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestGenerationCacheReusesImmutableRowsAndReplacesItsEntry(t *testing.T) {
	t.Parallel()
	set := document.VectorSetV1{VectorSpaceFingerprint: strings.Repeat("f", 64), Metric: document.VectorMetricDotProduct, Normalization: document.VectorNormalizationNone, Dimension: 2, InputKeys: []string{"row", "tail"}, InputChecksums: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}, Vectors: [][]float32{{1, 0}, {0, 1}}}
	_, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{setID})
	require.NoError(t, err)
	built, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	var cache vectorindex.GenerationCache
	loads := 0
	load := func() ([]byte, error) { loads++; return built.Bytes(), nil }
	first, err := cache.Get("first", load)
	require.NoError(t, err)
	identity := vectorindex.RowIdentity{SetID: setID, InputKey: "row", InputChecksum: strings.Repeat("a", 64)}
	vector := first.RowVectors([]vectorindex.RowIdentity{identity})[identity]
	for range 3 {
		opened, err := cache.Get("first", load)
		require.NoError(t, err)
		view := opened.RowVectors([]vectorindex.RowIdentity{identity})[identity]
		require.Equal(t, []float32{1, 0}, view)
		require.Same(t, &vector[0], &view[0], "warm reads must share immutable vector storage")
		require.Equal(t, len(view), cap(view), "appending must not overwrite another row")
	}
	require.Equal(t, 1, loads, "warm reads must avoid fetching the persisted payload")
	_, err = cache.Get("broken", func() ([]byte, error) { return nil, errors.New("storage unavailable") })
	require.ErrorContains(t, err, "storage unavailable")
	_, err = cache.Get("first", load)
	require.NoError(t, err)
	require.Equal(t, 1, loads, "a failed replacement keeps the prior immutable entry")
	_, err = cache.Get("second", load)
	require.NoError(t, err)
	_, err = cache.Get("first", load)
	require.NoError(t, err)
	require.Equal(t, 3, loads, "one retained generation bounds memory across replacements")
	require.Equal(t, []float32{1, 0}, vector, "views retain their storage after cache replacement")
	cache.Clear()
	_, err = cache.Get("first", load)
	require.NoError(t, err)
	require.Equal(t, 4, loads)
}
