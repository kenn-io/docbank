package vectorindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

type syntheticCorpus struct {
	manifest Manifest
	sets     []document.VectorSetV1
	queries  [][]float32
}

// Exact recall must be unchanged by the physical layout of an open generation.
func TestLayoutRecallAgainstIndependentExact(t *testing.T) {
	corpus := newSyntheticCorpus(t, 512, 32, 64, 12)
	built, err := BuildGeneration(corpus.manifest, corpus.sets, Options{})
	require.NoError(t, err)
	encoded := built.Bytes()
	opened, err := OpenGeneration(bytes.NewReader(encoded), int64(len(encoded)))
	require.NoError(t, err)
	for queryIndex, query := range corpus.queries {
		want := independentExactDot(corpus.manifest, corpus.sets, query, 20)
		for _, generation := range []*Generation{built, opened} {
			neighbors, err := generation.Search(query, 20)
			require.NoError(t, err)
			assert.Equal(t, neighborIdentities(want), neighborIdentities(neighbors), "query %d", queryIndex)
		}
	}
}

// Build includes serialization, so it measures the complete persistence path.
func BenchmarkLayoutBuild(b *testing.B) {
	corpus := newSyntheticCorpus(b, 10_000, 128, 1_000, 1)
	b.Run("row-major-generation", func(b *testing.B) {
		b.ReportAllocs()
		var encoded []byte
		for b.Loop() {
			generation, err := BuildGeneration(corpus.manifest, corpus.sets, Options{})
			if err != nil {
				b.Fatal(err)
			}
			encoded = generation.Bytes()
			runtime.KeepAlive(generation)
		}
		b.ReportMetric(float64(len(encoded)), "serialized-B")
		runtime.KeepAlive(encoded)
	})
}

func BenchmarkLayoutQuery(b *testing.B) {
	corpus := newSyntheticCorpus(b, 10_000, 128, 1_000, 16)
	b.Run("row-major-generation", func(b *testing.B) {
		generation, err := BuildGeneration(corpus.manifest, corpus.sets, Options{})
		require.NoError(b, err)
		b.ReportAllocs()
		queryIndex := 0
		for b.Loop() {
			neighbors, err := generation.Search(corpus.queries[queryIndex%len(corpus.queries)], 20)
			if err != nil {
				b.Fatal(err)
			}
			queryIndex++
			runtime.KeepAlive(neighbors)
		}
	})
}

func newSyntheticCorpus(tb testing.TB, rowCount, dimension, rowsPerSet, queryCount int) syntheticCorpus {
	tb.Helper()
	if rowCount < 1 || dimension < 1 || rowsPerSet < 1 || rowCount%rowsPerSet != 0 {
		tb.Fatal("synthetic corpus dimensions must be positive and evenly divisible")
	}
	random := rand.New(rand.NewPCG(0x5eed, 0xc0ffee)) //nolint:gosec // Reproducible synthetic benchmark data, not security randomness.
	sets := make([]document.VectorSetV1, rowCount/rowsPerSet)
	for setIndex := range sets {
		keys := make([]string, rowsPerSet)
		checksums := make([]string, rowsPerSet)
		vectors := make([][]float32, rowsPerSet)
		for localRow := range rowsPerSet {
			globalRow := setIndex*rowsPerSet + localRow
			keys[localRow] = fmt.Sprintf("synthetic-row-%06d", globalRow)
			checksums[localRow] = syntheticFingerprint(keys[localRow])
			vectors[localRow] = make([]float32, dimension)
			for scalar := range dimension {
				vectors[localRow][scalar] = float32(random.Float64()*2 - 1)
			}
		}
		sets[setIndex] = document.VectorSetV1{
			VectorSpaceFingerprint: syntheticFingerprint("synthetic-vector-space"),
			Metric:                 document.VectorMetricDotProduct, Normalization: document.VectorNormalizationNone,
			Dimension: dimension, InputKeys: keys, InputChecksums: checksums, Vectors: vectors,
		}
	}
	manifest := testManifest(tb, sets)
	queries := make([][]float32, queryCount)
	for queryIndex := range queries {
		queries[queryIndex] = make([]float32, dimension)
		for scalar := range dimension {
			queries[queryIndex][scalar] = float32(random.Float64()*2 - 1)
		}
	}
	return syntheticCorpus{manifest: manifest, sets: sets, queries: queries}
}

func syntheticFingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func independentExactDot(manifest Manifest, sets []document.VectorSetV1, query []float32, k int) []Neighbor {
	byID := make(map[string]document.VectorSetV1, len(sets))
	for _, set := range sets {
		_, setID, _ := document.EncodeVectorSetV1(set)
		byID[setID] = set
	}
	var neighbors []Neighbor
	for _, setID := range manifest.SetIDs {
		set := byID[setID]
		for row, vector := range set.Vectors {
			score := 0.0
			for scalar, queryValue := range query {
				score += float64(queryValue) * float64(vector[scalar])
			}
			neighbors = append(neighbors, Neighbor{
				SetID: setID, InputKey: set.InputKeys[row], InputChecksum: set.InputChecksums[row], Score: score})
		}
	}
	sort.Slice(neighbors, func(left, right int) bool {
		if neighbors[left].Score != neighbors[right].Score {
			return neighbors[left].Score > neighbors[right].Score
		}
		return compareIdentity(neighbors[left].RowIdentity, neighbors[right].RowIdentity) < 0
	})
	return neighbors[:k]
}

func neighborIdentities(neighbors []Neighbor) []RowIdentity {
	identities := make([]RowIdentity, len(neighbors))
	for index, neighbor := range neighbors {
		identities[index] = neighbor.RowIdentity
	}
	return identities
}

// BenchmarkLayoutOpen measures validation and decoding independently of build.
func BenchmarkLayoutOpen(b *testing.B) {
	corpus := newSyntheticCorpus(b, 10_000, 128, 1_000, 1)
	built, err := BuildGeneration(corpus.manifest, corpus.sets, Options{})
	require.NoError(b, err)
	encoded := built.Bytes()
	b.ReportAllocs()
	for b.Loop() {
		opened, err := OpenGeneration(bytes.NewReader(encoded), int64(len(encoded)))
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(opened)
	}
	b.StopTimer()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	opened, err := OpenGeneration(bytes.NewReader(encoded), int64(len(encoded)))
	require.NoError(b, err)
	runtime.GC()
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-B")
	runtime.KeepAlive(encoded)
	runtime.KeepAlive(opened)
}
