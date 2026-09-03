package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

const (
	vectorSetV1Magic    = "vector-set/v1\x00"
	vectorSetV1Domain   = "docbank-vector-set/v1\x00"
	vectorSetV1Version  = uint32(1)
	maxVectorSetString  = 1 << 16
	maxVectorSetScalars = 10_000_000
)

const (
	VectorNormalizationNone       = "none"
	VectorNormalizationUnitLength = "unit_length"
)

// VectorSetV1 is the immutable vector payload apart from its storage record.
type VectorSetV1 struct {
	VectorSpaceFingerprint string      `json:"vector_space_fingerprint"`
	Metric                 string      `json:"metric"`
	Normalization          string      `json:"normalization"`
	Dimension              int         `json:"dimension"`
	InputKeys              []string    `json:"input_keys"`
	InputChecksums         []string    `json:"input_checksums"`
	Vectors                [][]float32 `json:"vectors"`
}

// VectorSetV1Input accepts provider precision before it crosses the durable
// float32 boundary. Conversion follows IEEE-754 round-to-nearest-even.
type VectorSetV1Input struct {
	VectorSpaceFingerprint string
	Metric                 string
	Normalization          string
	Dimension              int
	InputKeys              []string
	InputChecksums         []string
	Values                 [][]float64
}

// NewVectorSetV1 constructs canonical float32 rows from provider float64
// values. It rejects values that are non-finite before or after quantization.
func NewVectorSetV1(input VectorSetV1Input) (VectorSetV1, error) {
	if err := preflightVectorSetInput(input); err != nil {
		return VectorSetV1{}, err
	}
	set := VectorSetV1{VectorSpaceFingerprint: input.VectorSpaceFingerprint, Metric: input.Metric, Normalization: input.Normalization, Dimension: input.Dimension, InputKeys: append([]string(nil), input.InputKeys...), InputChecksums: append([]string(nil), input.InputChecksums...), Vectors: make([][]float32, len(input.Values))}
	for row, values := range input.Values {
		set.Vectors[row] = make([]float32, len(values))
		for column, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return VectorSetV1{}, errors.New("vector input contains non-finite scalar")
			}
			quantized := float32(value)
			if math.IsInf(float64(quantized), 0) {
				return VectorSetV1{}, errors.New("vector input exceeds finite float32 range")
			}
			if quantized == 0 {
				quantized = 0
			}
			set.Vectors[row][column] = quantized
		}
	}
	if err := validateVectorSetV1(set); err != nil {
		return VectorSetV1{}, err
	}
	return set, nil
}

func preflightVectorSetInput(input VectorSetV1Input) error {
	if err := validateFingerprint(input.VectorSpaceFingerprint, "vector-space fingerprint"); err != nil {
		return err
	}
	if !IsValidVectorMetric(input.Metric) {
		return errors.New("vector metric is invalid")
	}
	if !validVectorNormalization(input.Normalization) {
		return errors.New("vector normalization is invalid")
	}
	if input.Dimension < 1 || input.Dimension > maxEmbeddingDimensions || len(input.Values) == 0 || len(input.Values) > maxEmbeddingBatchItems || len(input.InputKeys) != len(input.Values) || len(input.InputChecksums) != len(input.Values) || uint64(len(input.Values))*uint64(input.Dimension) > maxVectorSetScalars {
		return errors.New("vector input allocation exceeds bounds")
	}
	seen := make(map[string]struct{}, len(input.InputKeys))
	for index, row := range input.Values {
		if len(row) != input.Dimension {
			return errors.New("vector input row dimension does not match header")
		}
		if err := validateVectorSetKey(input.InputKeys[index]); err != nil {
			return err
		}
		if _, exists := seen[input.InputKeys[index]]; exists {
			return errors.New("vector input keys must be unique")
		}
		seen[input.InputKeys[index]] = struct{}{}
		if err := validateFingerprint(input.InputChecksums[index], "vector input checksum"); err != nil {
			return err
		}
		for _, value := range row {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return errors.New("vector input contains non-finite scalar")
			}
			if math.IsInf(float64(float32(value)), 0) {
				return errors.New("vector input exceeds finite float32 range")
			}
		}
	}
	return nil
}

// VectorBounds limits untrusted durable payloads before their rows or vectors
// are allocated. All fields must be positive for a decode operation.
type VectorBounds struct {
	MaxRows      int
	MaxDimension int
	MaxBytes     int
}

// EncodeVectorSetV1 emits the platform-independent vector-set/v1 framing and
// a SHA-256 over its domain prefix followed by the exact emitted bytes.
func EncodeVectorSetV1(set VectorSetV1) ([]byte, string, error) {
	if err := validateVectorSetV1(set); err != nil {
		return nil, "", err
	}
	var buffer bytes.Buffer
	buffer.WriteString(vectorSetV1Magic)
	writeUint32(&buffer, vectorSetV1Version)
	for _, value := range []string{set.VectorSpaceFingerprint, set.Metric, set.Normalization} {
		if err := writeString(&buffer, value); err != nil {
			return nil, "", err
		}
	}
	if err := writeBoundedUint32(&buffer, len(set.Vectors)); err != nil {
		return nil, "", err
	}
	if err := writeBoundedUint32(&buffer, set.Dimension); err != nil {
		return nil, "", err
	}
	for index := range set.Vectors {
		if err := writeString(&buffer, set.InputKeys[index]); err != nil {
			return nil, "", err
		}
		if err := writeString(&buffer, set.InputChecksums[index]); err != nil {
			return nil, "", err
		}
	}
	for _, vector := range set.Vectors {
		for _, value := range vector {
			if value == 0 {
				value = 0
			}
			writeUint32(&buffer, math.Float32bits(value))
		}
	}
	encoded := buffer.Bytes()
	checksum := vectorSetChecksum(encoded)
	return encoded, checksum, nil
}

// DecodeVectorSetV1 validates and reads one exactly framed vector-set/v1
// payload and returns the checksum EncodeVectorSetV1 produced for those exact
// bytes. Callers holding a stored checksum must compare it; a syntactically
// valid payload with altered scalars still decodes.
func DecodeVectorSetV1(encoded []byte, bounds VectorBounds) (VectorSetV1, string, error) {
	if bounds.MaxRows < 1 || bounds.MaxDimension < 1 || bounds.MaxBytes < 1 {
		return VectorSetV1{}, "", errors.New("vector bounds must permit positive rows, dimension, and bytes")
	}
	if len(encoded) > bounds.MaxBytes {
		return VectorSetV1{}, "", errors.New("vector payload exceeds byte bounds")
	}
	set, err := decodeVectorSetV1(encoded, bounds)
	if err != nil {
		return VectorSetV1{}, "", err
	}
	return set, vectorSetChecksum(encoded), nil
}

func decodeVectorSetV1(encoded []byte, bounds VectorBounds) (VectorSetV1, error) {
	reader := bytes.NewReader(encoded)
	magic := make([]byte, len(vectorSetV1Magic))
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != vectorSetV1Magic {
		return VectorSetV1{}, errors.New("vector payload has an invalid header")
	}
	if version, err := readUint32(reader); err != nil || version != vectorSetV1Version {
		return VectorSetV1{}, errors.New("vector payload has an unsupported version")
	}
	space, err := readString(reader)
	if err != nil {
		return VectorSetV1{}, err
	}
	metric, err := readString(reader)
	if err != nil {
		return VectorSetV1{}, err
	}
	normalization, err := readString(reader)
	if err != nil {
		return VectorSetV1{}, err
	}
	if err := validateFingerprint(space, "vector-space fingerprint"); err != nil {
		return VectorSetV1{}, err
	}
	if !IsValidVectorMetric(metric) || !validVectorNormalization(normalization) {
		return VectorSetV1{}, errors.New("vector payload has an invalid header")
	}
	rows, err := readUint32(reader)
	if err != nil {
		return VectorSetV1{}, err
	}
	dimension, err := readUint32(reader)
	if err != nil {
		return VectorSetV1{}, err
	}
	if rows == 0 || int64(rows) > int64(bounds.MaxRows) || rows > maxEmbeddingBatchItems {
		return VectorSetV1{}, errors.New("vector payload rows exceed bounds")
	}
	if dimension == 0 || int64(dimension) > int64(bounds.MaxDimension) || dimension > maxEmbeddingDimensions {
		return VectorSetV1{}, errors.New("vector payload dimension exceeds bounds")
	}
	scalars := uint64(rows) * uint64(dimension)
	if scalars > maxVectorSetScalars {
		return VectorSetV1{}, errors.New("vector payload allocation exceeds bounds")
	}
	metadata := *reader
	if err := preflightVectorMetadata(&metadata, rows, scalars); err != nil {
		return VectorSetV1{}, err
	}
	set := VectorSetV1{VectorSpaceFingerprint: space, Metric: metric, Normalization: normalization, Dimension: int(dimension), InputKeys: make([]string, int(rows)), InputChecksums: make([]string, int(rows)), Vectors: make([][]float32, int(rows))}
	for index := range set.Vectors {
		key, err := readString(reader)
		if err != nil {
			return VectorSetV1{}, err
		}
		checksum, err := readString(reader)
		if err != nil {
			return VectorSetV1{}, err
		}
		if err := validateVectorSetKey(key); err != nil {
			return VectorSetV1{}, err
		}
		if err := validateFingerprint(checksum, "vector input checksum"); err != nil {
			return VectorSetV1{}, err
		}
		set.InputKeys[index], set.InputChecksums[index] = key, checksum
	}
	for index := range set.Vectors {
		set.Vectors[index] = make([]float32, int(dimension))
		for scalar := range set.Vectors[index] {
			bits, err := readUint32(reader)
			if err != nil {
				return VectorSetV1{}, err
			}
			value := math.Float32frombits(bits)
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return VectorSetV1{}, errors.New("vector payload contains non-finite scalar")
			}
			if value == 0 && bits != 0 {
				return VectorSetV1{}, errors.New("vector payload contains non-canonical negative zero")
			}
			set.Vectors[index][scalar] = value
		}
	}
	if reader.Len() != 0 {
		return VectorSetV1{}, errors.New("vector payload has trailing bytes")
	}
	if err := validateVectorSetV1(set); err != nil {
		return VectorSetV1{}, err
	}
	return set, nil
}

// preflightVectorMetadata proves the entire remaining frame is canonical before
// DecodeVectorSetV1 allocates the caller-selected rows and scalar buffers.
func preflightVectorMetadata(reader *bytes.Reader, rows uint32, scalars uint64) error {
	seen := make(map[string]struct{}, rows)
	for range rows {
		key, err := readString(reader)
		if err != nil {
			return err
		}
		checksum, err := readString(reader)
		if err != nil {
			return err
		}
		if err := validateVectorSetKey(key); err != nil {
			return err
		}
		if _, exists := seen[key]; exists {
			return errors.New("vector input keys must be unique")
		}
		seen[key] = struct{}{}
		if err := validateFingerprint(checksum, "vector input checksum"); err != nil {
			return err
		}
	}
	for range scalars {
		bits, err := readUint32(reader)
		if err != nil {
			return err
		}
		value := math.Float32frombits(bits)
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("vector payload contains non-finite scalar")
		}
		if value == 0 && bits != 0 {
			return errors.New("vector payload contains non-canonical negative zero")
		}
	}
	if reader.Len() != 0 {
		return errors.New("vector payload scalar bytes do not match header")
	}
	return nil
}

func validateVectorSetV1(set VectorSetV1) error {
	if err := validateFingerprint(set.VectorSpaceFingerprint, "vector-space fingerprint"); err != nil {
		return err
	}
	if !IsValidVectorMetric(set.Metric) {
		return errors.New("vector metric is invalid")
	}
	if !validVectorNormalization(set.Normalization) {
		return errors.New("vector normalization is invalid")
	}
	if set.Dimension < 1 || set.Dimension > maxEmbeddingDimensions {
		return fmt.Errorf("vector dimension must be between 1 and %d", maxEmbeddingDimensions)
	}
	if len(set.Vectors) == 0 || len(set.Vectors) > maxEmbeddingBatchItems || len(set.InputKeys) != len(set.Vectors) || len(set.InputChecksums) != len(set.Vectors) {
		return errors.New("vector rows, keys, and checksums must have equal non-zero length")
	}
	if uint64(len(set.Vectors))*uint64(set.Dimension) > maxVectorSetScalars {
		return errors.New("vector scalar allocation exceeds bounds")
	}
	seen := make(map[string]struct{}, len(set.InputKeys))
	for index, vector := range set.Vectors {
		if err := validateVectorSetKey(set.InputKeys[index]); err != nil {
			return err
		}
		if _, exists := seen[set.InputKeys[index]]; exists {
			return errors.New("vector input keys must be unique")
		}
		seen[set.InputKeys[index]] = struct{}{}
		if err := validateFingerprint(set.InputChecksums[index], "vector input checksum"); err != nil {
			return err
		}
		if len(vector) != set.Dimension {
			return errors.New("vector row dimension does not match header")
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return errors.New("vector contains non-finite scalar")
			}
		}
	}
	return nil
}

func validVectorNormalization(value string) bool {
	return value == VectorNormalizationNone || value == VectorNormalizationUnitLength
}

func validateVectorSetKey(value string) error {
	if value == "" || len(value) > maxVectorSetString || !utf8.ValidString(value) {
		return errors.New("vector input key must be bounded non-empty UTF-8")
	}
	return nil
}
func vectorSetChecksum(encoded []byte) string {
	digest := sha256.Sum256(append([]byte(vectorSetV1Domain), encoded...))
	return hex.EncodeToString(digest[:])
}
func writeUint32(buffer *bytes.Buffer, value uint32) {
	var data [4]byte
	binary.LittleEndian.PutUint32(data[:], value)
	buffer.Write(data[:])
}
func writeString(buffer *bytes.Buffer, value string) error {
	if err := writeBoundedUint32(buffer, len(value)); err != nil {
		return err
	}
	buffer.WriteString(value)
	return nil
}
func writeBoundedUint32(buffer *bytes.Buffer, value int) error {
	if value < 0 || uint64(value) > math.MaxUint32 {
		return errors.New("vector value cannot fit uint32 framing")
	}
	writeUint32(buffer, uint32(value))
	return nil
}
func readUint32(reader *bytes.Reader) (uint32, error) {
	var data [4]byte
	if _, err := io.ReadFull(reader, data[:]); err != nil {
		return 0, errors.New("vector payload is truncated")
	}
	return binary.LittleEndian.Uint32(data[:]), nil
}
func readString(reader *bytes.Reader) (string, error) {
	length, err := readUint32(reader)
	if err != nil {
		return "", err
	}
	if length == 0 || length > maxVectorSetString || int64(length) > int64(reader.Len()) {
		return "", errors.New("vector payload has invalid length-prefixed string")
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", errors.New("vector payload is truncated")
	}
	if !utf8.Valid(data) {
		return "", errors.New("vector payload string is not valid UTF-8")
	}
	return string(data), nil
}
