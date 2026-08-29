package vectorindex

import (
	"bytes"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// This test fails if row checksums are incorrectly treated as part of logical
// identity, allowing one (set ID, input key) row to appear twice.
func TestOpenGenerationRejectsDuplicateLogicalRowsWithDifferentChecksums(t *testing.T) {
	setID := strings.Repeat("a", 64)
	manifest, err := NewManifest([]string{setID})
	require.NoError(t, err)
	rows := []generationRow{
		{SetID: setID, InputKey: "duplicate", InputChecksum: strings.Repeat("b", 64), vector: []float32{1}},
		{SetID: setID, InputKey: "duplicate", InputChecksum: strings.Repeat("c", 64), vector: []float32{2}},
	}
	encoded, err := encodeGeneration(manifest, strings.Repeat("f", 64), document.VectorMetricDotProduct,
		document.VectorNormalizationNone, 1, rows, defaultMaxBytes)
	require.NoError(t, err)

	_, err = OpenGeneration(bytes.NewReader(encoded), int64(len(encoded)))
	require.ErrorContains(t, err, "duplicate logical rows")
}

func TestBuildGenerationRejectsInvalidSourceAuthority(t *testing.T) {
	base := testVectorSet(document.VectorMetricCosine, document.VectorNormalizationNone,
		[]string{"row-a"}, [][]float32{{1, 0}})
	other := testVectorSet(document.VectorMetricCosine, document.VectorNormalizationNone,
		[]string{"row-b"}, [][]float32{{0, 1}})
	unexpected := testVectorSet(document.VectorMetricCosine, document.VectorNormalizationNone,
		[]string{"row-c"}, [][]float32{{1, 1}})
	validManifest := testManifest(t, []document.VectorSetV1{base, other})

	tests := []struct {
		name     string
		manifest Manifest
		sets     []document.VectorSetV1
		options  Options
		want     string
	}{
		{
			name: "unsorted manifest",
			manifest: Manifest{SetIDs: slices.Clone(validManifest.SetIDs),
				Checksum: manifestChecksum(validManifest.SetIDs)},
			sets: []document.VectorSetV1{base, other}, want: "canonically sorted",
		},
		{
			name:     "wrong manifest checksum",
			manifest: Manifest{SetIDs: validManifest.SetIDs, Checksum: strings.Repeat("0", 64)},
			sets:     []document.VectorSetV1{base, other}, want: "checksum mismatch",
		},
		{
			name: "missing named set", manifest: validManifest,
			sets: []document.VectorSetV1{base, unexpected}, want: "missing vector set",
		},
		{
			name: "mixed vector spaces", manifest: testManifest(t, []document.VectorSetV1{base, withSpace(other, strings.Repeat("e", 64))}),
			sets: []document.VectorSetV1{base, withSpace(other, strings.Repeat("e", 64))}, want: "incompatible vector spaces",
		},
		{
			name: "incompatible metric descriptor", manifest: testManifest(t, []document.VectorSetV1{base, withMetric(other, document.VectorMetricDotProduct)}),
			sets: []document.VectorSetV1{base, withMetric(other, document.VectorMetricDotProduct)}, want: "incompatible vector spaces",
		},
		{
			name: "dimension bound", manifest: testManifest(t, []document.VectorSetV1{base}),
			sets: []document.VectorSetV1{base}, options: Options{MaxDimension: 1}, want: "allocation exceeds build bounds",
		},
		{
			name: "row bound", manifest: testManifest(t, []document.VectorSetV1{base, other}),
			sets: []document.VectorSetV1{base, other}, options: Options{MaxRows: 1}, want: "allocation exceeds build bounds",
		},
		{
			name: "byte bound", manifest: testManifest(t, []document.VectorSetV1{base}),
			sets: []document.VectorSetV1{base}, options: Options{MaxBytes: 128}, want: "exceeds byte bounds",
		},
		{
			name: "zero cosine", manifest: testManifest(t, []document.VectorSetV1{withVectors(base, [][]float32{{0, 0}})}),
			sets: []document.VectorSetV1{withVectors(base, [][]float32{{0, 0}})}, want: "must be non-zero",
		},
		{
			name: "false unit normalization", manifest: testManifest(t, []document.VectorSetV1{withNormalization(withVectors(base, [][]float32{{2, 0}}), document.VectorNormalizationUnitLength)}),
			sets: []document.VectorSetV1{withNormalization(withVectors(base, [][]float32{{2, 0}}), document.VectorNormalizationUnitLength)}, want: "unit-length",
		},
		{
			name: "non-finite", manifest: validManifest,
			sets: []document.VectorSetV1{withVectors(base, [][]float32{{float32(math.NaN()), 0}}), other}, want: "non-finite",
		},
	}

	// Reverse the otherwise valid IDs without changing their checksum formula so
	// this case reaches the canonical-order check rather than checksum validation.
	slices.Reverse(tests[0].manifest.SetIDs)
	tests[0].manifest.Checksum = manifestChecksum(tests[0].manifest.SetIDs)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildGeneration(test.manifest, test.sets, test.options)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestBuilderEnforcesCumulativeBoundsBeforeRetainingNextSet(t *testing.T) {
	first := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{"row-a"}, [][]float32{{1}})
	second := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{"row-b"}, [][]float32{{2}})
	manifest := testManifest(t, []document.VectorSetV1{first, second})
	builder, err := NewBuilder(manifest, Options{MaxRows: 1})
	require.NoError(t, err)
	_, firstID, err := document.EncodeVectorSetV1(first)
	require.NoError(t, err)
	require.NoError(t, builder.Add(firstID, first))
	_, secondID, err := document.EncodeVectorSetV1(second)
	require.NoError(t, err)
	err = builder.Add(secondID, second)
	require.ErrorContains(t, err, "allocation exceeds build bounds")
	assert.Equal(t, 1, builder.RowCount(), "failed cumulative admission must retain no rows")
}

// This test fails if subtracting the checksum footer from a caller's tiny
// unsigned byte bound underflows and admits a generation larger than the bound.
func TestBuildGenerationRejectsEveryByteBoundSmallerThanChecksum(t *testing.T) {
	set := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{"row-a"}, [][]float32{{1}})
	manifest := testManifest(t, []document.VectorSetV1{set})
	for maxBytes := int64(1); maxBytes < generationChecksumBytes; maxBytes++ {
		_, err := BuildGeneration(manifest, []document.VectorSetV1{set}, Options{MaxBytes: maxBytes})
		require.ErrorContains(t, err, "exceeds byte bounds", "MaxBytes=%d", maxBytes)
	}
	largeMetadataSet := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{strings.Repeat("k", maxIdentityBytes)}, [][]float32{{1}})
	_, err := BuildGeneration(testManifest(t, []document.VectorSetV1{largeMetadataSet}),
		[]document.VectorSetV1{largeMetadataSet}, Options{MaxBytes: 1 << 10})
	require.ErrorContains(t, err, "exceeds byte bounds")
}

func TestSearchRejectsInvalidBoundsAndQueries(t *testing.T) {
	set := testVectorSet(document.VectorMetricCosine, document.VectorNormalizationNone,
		[]string{"row-a", "row-b"}, [][]float32{{1, 0}, {0, 1}})
	generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}),
		[]document.VectorSetV1{set}, Options{})
	require.NoError(t, err)

	tests := []struct {
		name   string
		query  []float32
		k      int
		visits int
		want   string
	}{
		{name: "zero k", query: []float32{1, 0}, k: 0, visits: 2, want: "k is outside"},
		{name: "oversized k", query: []float32{1, 0}, k: 3, visits: 3, want: "k is outside"},
		{name: "short visit bound", query: []float32{1, 0}, k: 1, visits: 1, want: "visit bound"},
		{name: "long visit bound", query: []float32{1, 0}, k: 1, visits: 3, want: "visit bound"},
		{name: "wrong dimension", query: []float32{1}, k: 1, visits: 2, want: "dimension"},
		{name: "non-finite", query: []float32{float32(math.Inf(1)), 0}, k: 1, visits: 2, want: "non-finite"},
		{name: "zero cosine", query: []float32{0, 0}, k: 1, visits: 2, want: "must be non-zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := generation.Search(test.query, test.k, test.visits)
			require.ErrorContains(t, err, test.want)
		})
	}
	var unopened *Generation
	_, err = unopened.Search([]float32{1, 0}, 1, 2)
	require.ErrorContains(t, err, "not open")
}

func TestOpenGenerationRejectsCorruptionTruncationAndTrailingBytes(t *testing.T) {
	set := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{"row-a", "row-b"}, [][]float32{{1, 2}, {3, 4}})
	generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}),
		[]document.VectorSetV1{set}, Options{})
	require.NoError(t, err)
	encoded := generation.Bytes()
	fieldsOffset := len(generationDomain) + 8
	metadataOffset := binary.LittleEndian.Uint64(encoded[fieldsOffset:])
	vectorOffset := binary.LittleEndian.Uint64(encoded[fieldsOffset+16:])
	checksumOffset := binary.LittleEndian.Uint64(encoded[fieldsOffset+32:])

	for _, location := range []int{0, int(metadataOffset) + 4, int(vectorOffset), int(checksumOffset)} {
		corrupt := slices.Clone(encoded)
		corrupt[location] ^= 1
		_, err := OpenGeneration(bytes.NewReader(corrupt), int64(len(corrupt)))
		require.Error(t, err, "bit flip at byte %d was accepted", location)
	}

	for cut := range encoded {
		_, err := OpenGeneration(bytes.NewReader(encoded[:cut]), int64(cut))
		require.Error(t, err, "truncation at byte %d was accepted", cut)
	}
	withTrailing := append(slices.Clone(encoded), 0)
	_, err = OpenGeneration(bytes.NewReader(withTrailing), int64(len(withTrailing)))
	require.ErrorContains(t, err, "sections")
	_, err = OpenGeneration(bytes.NewReader(withTrailing), int64(len(encoded)))
	require.ErrorContains(t, err, "trailing bytes")

	overlap := slices.Clone(encoded)
	binary.LittleEndian.PutUint64(overlap[fieldsOffset+16:], metadataOffset)
	rewriteGenerationChecksum(overlap, checksumOffset)
	_, err = OpenGeneration(bytes.NewReader(overlap), int64(len(overlap)))
	require.ErrorContains(t, err, "overlapping sections")

	oversizedCount := slices.Clone(encoded)
	binary.LittleEndian.PutUint64(oversizedCount[fieldsOffset+40:], math.MaxUint64)
	rewriteGenerationChecksum(oversizedCount, checksumOffset)
	_, err = OpenGeneration(bytes.NewReader(oversizedCount), int64(len(oversizedCount)))
	require.ErrorContains(t, err, "dimensions exceed bounds")

	invalidUTF8 := slices.Clone(encoded)
	keyOffset := bytes.Index(invalidUTF8, []byte("row-a"))
	require.NotEqual(t, -1, keyOffset)
	invalidUTF8[keyOffset] = 0xff
	rewriteGenerationChecksum(invalidUTF8, checksumOffset)
	_, err = OpenGeneration(bytes.NewReader(invalidUTF8), int64(len(invalidUTF8)))
	require.ErrorContains(t, err, "input identity")

	negativeZero := slices.Clone(encoded)
	binary.LittleEndian.PutUint32(negativeZero[vectorOffset:], math.Float32bits(float32(math.Copysign(0, -1))))
	rewriteGenerationChecksum(negativeZero, checksumOffset)
	_, err = OpenGeneration(bytes.NewReader(negativeZero), int64(len(negativeZero)))
	require.ErrorContains(t, err, "non-canonical vector scalars")
}

func TestBytesReturnsIndependentCopy(t *testing.T) {
	set := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{"row-a"}, [][]float32{{1}})
	generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}),
		[]document.VectorSetV1{set}, Options{})
	require.NoError(t, err)
	first := generation.Bytes()
	first[0] ^= 1
	assert.NotEqual(t, first, generation.Bytes())
}

func withSpace(set document.VectorSetV1, space string) document.VectorSetV1 {
	set.VectorSpaceFingerprint = space
	return set
}

func withVectors(set document.VectorSetV1, vectors [][]float32) document.VectorSetV1 {
	set.Vectors = vectors
	return set
}

func withNormalization(set document.VectorSetV1, normalization string) document.VectorSetV1 {
	set.Normalization = normalization
	return set
}

func withMetric(set document.VectorSetV1, metric string) document.VectorSetV1 {
	set.Metric = metric
	return set
}

func rewriteGenerationChecksum(encoded []byte, checksumOffset uint64) {
	checksum := generationChecksum(encoded[:checksumOffset])
	copy(encoded[checksumOffset:], checksum[:])
}

// This test fails if a self-consistent projection checksum can replace the
// authoritative vector payload while retaining its canonical vector-set ID.
func TestOpenGenerationRejectsPayloadThatDoesNotMatchSourceSetIdentity(t *testing.T) {
	set := testVectorSet(document.VectorMetricDotProduct, document.VectorNormalizationNone,
		[]string{"row-a"}, [][]float32{{1, 2}})
	generation, err := BuildGeneration(testManifest(t, []document.VectorSetV1{set}),
		[]document.VectorSetV1{set}, Options{})
	require.NoError(t, err)
	forged := generation.Bytes()

	fieldsOffset := len(generationDomain) + 8
	vectorOffset := binary.LittleEndian.Uint64(forged[fieldsOffset+16:])
	checksumOffset := binary.LittleEndian.Uint64(forged[fieldsOffset+32:])
	forged[vectorOffset] ^= 1
	checksum := generationChecksum(forged[:checksumOffset])
	copy(forged[checksumOffset:], checksum[:])

	_, err = OpenGeneration(bytes.NewReader(forged), int64(len(forged)))
	require.ErrorContains(t, err, "source set identity")
}
