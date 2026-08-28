package vectorindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"testing"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

const transposedCandidateDomain = "docbank-vector-index-candidate/column-major/v1\x00"

type syntheticCorpus struct {
	manifest Manifest
	sets     []document.VectorSetV1
	queries  [][]float32
}

type transposedCandidate struct {
	identities []RowIdentity
	columns    [][]float32
}

// This test fails if the selected row-major backend or the alternative
// transposed layout loses any top-k neighbor against an independent exact scan.
func TestSelectedAndCandidateLayoutsRecallAgainstIndependentExact(t *testing.T) {
	corpus := newSyntheticCorpus(t, 512, 32, 64, 12)
	selected, err := BuildGeneration(corpus.manifest, corpus.sets, Options{})
	require.NoError(t, err)
	candidate, err := buildTransposedCandidate(corpus.manifest, corpus.sets)
	require.NoError(t, err)

	for queryIndex, query := range corpus.queries {
		want := independentExactDot(corpus.manifest, corpus.sets, query, 20)
		selectedNeighbors, searchErr := selected.Search(query, 20, 512)
		require.NoError(t, searchErr)
		candidateNeighbors := candidate.search(query, 20)
		assert.Equal(t, neighborIdentities(want), neighborIdentities(selectedNeighbors),
			"selected recall differs for query %d", queryIndex)
		assert.Equal(t, neighborIdentities(want), neighborIdentities(candidateNeighbors),
			"candidate recall differs for query %d", queryIndex)
	}
}

// This test fails if the comparison candidate lacks deterministic,
// architecture-safe bytes or cannot detect corruption of those bytes.
func TestTransposedCandidateSerializationIsDeterministicAndChecksummed(t *testing.T) {
	corpus := newSyntheticCorpus(t, 64, 8, 16, 1)
	first, err := buildTransposedCandidate(corpus.manifest, corpus.sets)
	require.NoError(t, err)
	reversed := slices.Clone(corpus.sets)
	slices.Reverse(reversed)
	second, err := buildTransposedCandidate(corpus.manifest, reversed)
	require.NoError(t, err)
	firstBytes, secondBytes := first.bytes(), second.bytes()
	assert.Equal(t, firstBytes, secondBytes)
	require.NoError(t, validateTransposedCandidateBytes(firstBytes))
	corrupt := slices.Clone(firstBytes)
	corrupt[len(corrupt)/2] ^= 1
	require.ErrorContains(t, validateTransposedCandidateBytes(corrupt), "checksum")
}

func BenchmarkLayoutBuild(b *testing.B) {
	corpus := newSyntheticCorpus(b, 10_000, 128, 1_000, 1)
	b.Run("row-major-generation", func(b *testing.B) {
		b.ReportAllocs()
		resetBenchmarkMemory(b)
		var generation Generation
		for b.Loop() {
			var err error
			generation, err = BuildGeneration(corpus.manifest, corpus.sets, Options{})
			if err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(len(generation.encoded)), "serialized-B")
		reportProcessRSS(b)
		runtime.KeepAlive(generation)
	})
	b.Run("transposed-candidate", func(b *testing.B) {
		b.ReportAllocs()
		resetBenchmarkMemory(b)
		var candidate transposedCandidate
		serializedBytes := 0
		for b.Loop() {
			var err error
			candidate, err = buildTransposedCandidate(corpus.manifest, corpus.sets)
			if err != nil {
				b.Fatal(err)
			}
			encoded := candidate.bytes()
			serializedBytes = len(encoded)
			runtime.KeepAlive(encoded)
		}
		b.ReportMetric(float64(serializedBytes), "serialized-B")
		reportProcessRSS(b)
		runtime.KeepAlive(candidate)
	})
}

func BenchmarkLayoutQuery(b *testing.B) {
	corpus := newSyntheticCorpus(b, 10_000, 128, 1_000, 16)
	b.Run("row-major-generation", func(b *testing.B) {
		selected, err := BuildGeneration(corpus.manifest, corpus.sets, Options{})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		resetBenchmarkMemory(b)
		queryIndex := 0
		for b.Loop() {
			query := corpus.queries[queryIndex%len(corpus.queries)]
			queryIndex++
			neighbors, searchErr := selected.Search(query, 20, 10_000)
			if searchErr != nil {
				b.Fatal(searchErr)
			}
			runtime.KeepAlive(neighbors)
		}
		b.ReportMetric(float64(len(selected.encoded)+10_000*128*4), "retained-B")
		reportProcessRSS(b)
		runtime.KeepAlive(selected)
	})
	b.Run("transposed-candidate", func(b *testing.B) {
		candidate, err := buildTransposedCandidate(corpus.manifest, corpus.sets)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		resetBenchmarkMemory(b)
		queryIndex := 0
		for b.Loop() {
			query := corpus.queries[queryIndex%len(corpus.queries)]
			queryIndex++
			neighbors := candidate.search(query, 20)
			runtime.KeepAlive(neighbors)
		}
		b.ReportMetric(float64(10_000*128*4), "retained-B")
		reportProcessRSS(b)
		runtime.KeepAlive(candidate)
	})
}

func resetBenchmarkMemory(b *testing.B) {
	b.Helper()
	b.StopTimer()
	runtime.GC()
	debug.FreeOSMemory()
	b.StartTimer()
}

func reportProcessRSS(b *testing.B) {
	b.Helper()
	b.StopTimer()
	current, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		b.Fatal(err)
	}
	memory, err := current.MemoryInfo()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(memory.RSS), "rss-B")
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

func buildTransposedCandidate(manifest Manifest, sets []document.VectorSetV1) (transposedCandidate, error) {
	if err := validateManifest(manifest); err != nil {
		return transposedCandidate{}, err
	}
	byID := make(map[string]document.VectorSetV1, len(sets))
	rowCount, dimension := 0, 0
	for _, set := range sets {
		_, setID, err := document.EncodeVectorSetV1(set)
		if err != nil {
			return transposedCandidate{}, err
		}
		byID[setID] = set
		rowCount += len(set.Vectors)
		if dimension == 0 {
			dimension = set.Dimension
		}
	}
	candidate := transposedCandidate{
		identities: make([]RowIdentity, 0, rowCount),
		columns:    make([][]float32, dimension),
	}
	for column := range candidate.columns {
		candidate.columns[column] = make([]float32, rowCount)
	}
	rowIndex := 0
	for _, setID := range manifest.SetIDs {
		set, exists := byID[setID]
		if !exists || set.Dimension != dimension || set.Metric != document.VectorMetricDotProduct {
			return transposedCandidate{}, errors.New("transposed candidate received incompatible source sets")
		}
		for localRow, vector := range set.Vectors {
			candidate.identities = append(candidate.identities, RowIdentity{
				SetID: setID, InputKey: set.InputKeys[localRow], InputChecksum: set.InputChecksums[localRow],
			})
			for column, value := range vector {
				candidate.columns[column][rowIndex] = value
			}
			rowIndex++
		}
	}
	return candidate, nil
}

func (candidate transposedCandidate) search(query []float32, k int) []Neighbor {
	neighbors := make([]Neighbor, len(candidate.identities))
	for index, identity := range candidate.identities {
		neighbors[index].RowIdentity = identity
	}
	for column, queryValue := range query {
		for row, value := range candidate.columns[column] {
			neighbors[row].Score += float64(queryValue) * float64(value)
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

func (candidate transposedCandidate) bytes() []byte {
	var output bytes.Buffer
	output.WriteString(transposedCandidateDomain)
	writeUint32(&output, uint32(len(candidate.identities)))
	writeUint32(&output, uint32(len(candidate.columns)))
	for _, identity := range candidate.identities {
		_ = writeString(&output, identity.SetID)
		_ = writeString(&output, identity.InputKey)
		_ = writeString(&output, identity.InputChecksum)
	}
	for _, column := range candidate.columns {
		for _, value := range column {
			writeUint32(&output, math.Float32bits(value))
		}
	}
	digest := sha256.Sum256(output.Bytes())
	output.Write(digest[:])
	return output.Bytes()
}

func validateTransposedCandidateBytes(encoded []byte) error {
	if len(encoded) < len(transposedCandidateDomain)+8+sha256.Size {
		return errors.New("transposed candidate is truncated")
	}
	payload, checksum := encoded[:len(encoded)-sha256.Size], encoded[len(encoded)-sha256.Size:]
	digest := sha256.Sum256(payload)
	if !bytes.Equal(digest[:], checksum) {
		return errors.New("transposed candidate checksum mismatch")
	}
	return nil
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
