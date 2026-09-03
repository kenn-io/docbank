package document_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// This test fails if a future codec change alters the durable bytes, including
// its header, framed identities, float byte order, or negative-zero handling.
func TestVectorSetV1EncodesCanonicalGoldenBytes(t *testing.T) {
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationUnitLength, Dimension: 2,
		InputKeys:      []string{"chunk-a", "chunk-b"},
		InputChecksums: []string{testFingerprint(), testFingerprint()},
		Values:         [][]float64{{math.Copysign(0, -1), 1 + math.Exp2(-24)}, {1 + 3*math.Exp2(-24), -2.5}},
	})
	require.NoError(t, err)
	got, checksum, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	want, err := os.ReadFile("testdata/vector-set-v1.golden.bin")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, "6aa367d8ac92d6893dd8768980ea06cc2024edf0822e58d34de2bafcdf65263f", checksum)
	assert.Equal(t, uint32(0), math.Float32bits(set.Vectors[0][0]))
	assert.Equal(t, uint32(0x3f800000), math.Float32bits(set.Vectors[0][1]))
	assert.Equal(t, uint32(0x3f800002), math.Float32bits(set.Vectors[1][0]))
	decoded, decodedChecksum, err := document.DecodeVectorSetV1(got, document.VectorBounds{MaxRows: 2, MaxDimension: 2, MaxBytes: len(got)})
	require.NoError(t, err)
	assert.False(t, math.Signbit(float64(decoded.Vectors[0][0])))
	assert.Equal(t, checksum, decodedChecksum)
}

// This test fails if a stored payload whose scalars were altered after
// encoding can decode without giving the caller a checksum that exposes it.
func TestVectorSetV1DecodeReportsChecksumOfAlteredPayload(t *testing.T) {
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationNone, Dimension: 1,
		InputKeys: []string{"chunk-a"}, InputChecksums: []string{testFingerprint()}, Values: [][]float64{{1}},
	})
	require.NoError(t, err)
	encoded, checksum, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	altered := append([]byte(nil), encoded...)
	binary.LittleEndian.PutUint32(altered[len(altered)-4:], math.Float32bits(2))

	decoded, alteredChecksum, err := document.DecodeVectorSetV1(altered, document.VectorBounds{MaxRows: 1, MaxDimension: 1, MaxBytes: len(altered)})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{2}}, decoded.Vectors)
	assert.NotEqual(t, checksum, alteredChecksum)
}

// This test fails if untrusted vector bytes can allocate beyond caller policy,
// retain a non-canonical negative zero, or ignore trailing bytes.
func TestVectorSetV1RejectsMalformedOrUnboundedBytes(t *testing.T) {
	set := document.VectorSetV1{
		VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationNone, Dimension: 1,
		InputKeys: []string{"chunk-a"}, InputChecksums: []string{testFingerprint()}, Vectors: [][]float32{{1}},
	}
	encoded, _, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)

	_, _, err = document.DecodeVectorSetV1(encoded, document.VectorBounds{MaxRows: 0, MaxDimension: 1, MaxBytes: len(encoded)})
	require.ErrorContains(t, err, "rows")
	_, _, err = document.DecodeVectorSetV1(append(encoded, 0), document.VectorBounds{MaxRows: 1, MaxDimension: 1, MaxBytes: len(encoded) + 1})
	require.ErrorContains(t, err, "scalar bytes")
}

// This test fails if vector-set framing cannot retain the same normalization
// identifier that a canonical embedding binding uses.
func TestVectorSetV1PreservesUnitLengthNormalization(t *testing.T) {
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationUnitLength, Dimension: 1,
		InputKeys: []string{"chunk-a"}, InputChecksums: []string{testFingerprint()}, Values: [][]float64{{1}},
	})
	require.NoError(t, err)
	assert.Equal(t, document.VectorNormalizationUnitLength, set.Normalization)
}

func TestVectorSetV1RejectsL2NormalizationAlias(t *testing.T) {
	_, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine,
		Normalization: "l2", Dimension: 1,
		InputKeys: []string{"chunk-a"}, InputChecksums: []string{testFingerprint()}, Values: [][]float64{{1}},
	})
	require.ErrorContains(t, err, "normalization")
}

func TestVectorSetV1RejectsEveryTruncatedFixedWidthField(t *testing.T) {
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, Dimension: 1, InputKeys: []string{"chunk-a"}, InputChecksums: []string{testFingerprint()}, Values: [][]float64{{1}}})
	require.NoError(t, err)
	encoded, _, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	for _, payload := range [][]byte{encoded[:3], encoded[:len(encoded)-1]} {
		_, _, err := document.DecodeVectorSetV1(payload, document.VectorBounds{MaxRows: 1, MaxDimension: 1, MaxBytes: len(encoded)})
		require.Error(t, err)
	}
}

func TestVectorSetV1PreflightsUntrustedFrameBeforeAllocatingRows(t *testing.T) {
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, Dimension: 1, InputKeys: []string{"chunk-a"}, InputChecksums: []string{testFingerprint()}, Values: [][]float64{{1}}})
	require.NoError(t, err)
	encoded, _, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)

	// Header framing is fixed through the two uint32 row/dimension fields.
	rowsOffset := 14 + 4 + 4 + 64 + 4 + len(document.VectorMetricCosine) + 4 + len(document.VectorNormalizationNone)
	oversizedRows := append([]byte(nil), encoded...)
	binary.LittleEndian.PutUint32(oversizedRows[rowsOffset:], ^uint32(0))
	_, _, err = document.DecodeVectorSetV1(oversizedRows, document.VectorBounds{MaxRows: int(^uint32(0)), MaxDimension: 1, MaxBytes: len(oversizedRows)})
	require.ErrorContains(t, err, "rows exceed bounds")

	invalidHeader := append([]byte(nil), encoded...)
	invalidHeader[14+4+4] = 'x'
	_, _, err = document.DecodeVectorSetV1(invalidHeader, document.VectorBounds{MaxRows: 1, MaxDimension: 1, MaxBytes: len(invalidHeader)})
	require.ErrorContains(t, err, "fingerprint")

	_, err = document.NewVectorSetV1(document.VectorSetV1Input{VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, Dimension: 1, InputKeys: make([]string, 10_001), InputChecksums: make([]string, 10_001), Values: make([][]float64, 10_001)})
	require.ErrorContains(t, err, "allocation exceeds bounds")
}

// This test fails if duplicate metadata can proceed to scalar validation or
// row allocation instead of being rejected during metadata preflight.
func TestVectorSetV1RejectsDuplicateMetadataKeysBeforeScalarAllocation(t *testing.T) {
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{VectorSpaceFingerprint: testFingerprint(), Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, Dimension: 1, InputKeys: []string{"chunk-a", "chunk-b"}, InputChecksums: []string{testFingerprint(), testFingerprint()}, Values: [][]float64{{1}, {2}}})
	require.NoError(t, err)
	encoded, _, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	duplicate := append([]byte(nil), encoded...)
	secondKey := bytes.LastIndex(duplicate, []byte("chunk-b"))
	require.GreaterOrEqual(t, secondKey, 0)
	duplicate[secondKey+len("chunk-")] = 'a'
	binary.LittleEndian.PutUint32(duplicate[len(duplicate)-4:], math.Float32bits(float32(math.NaN())))
	_, _, err = document.DecodeVectorSetV1(duplicate, document.VectorBounds{MaxRows: 2, MaxDimension: 1, MaxBytes: len(duplicate)})
	require.ErrorContains(t, err, "keys must be unique")
}
