// Package vectorindex builds disposable, deterministic search projections from
// canonical document vector sets.
package vectorindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	generationFormat        = "docbank-vector-index/v1"
	generationDomain        = generationFormat + "\x00"
	generationVersion       = uint32(1)
	generationLayout        = "exact-row-major-f32le/v1"
	generationChecksumBytes = 32
	maxIdentityBytes        = 1 << 16
	defaultMaxRows          = 1_000_000
	defaultMaxDimension     = 16_384
	defaultMaxBytes         = int64(512 << 20)
)

var generationHeaderSize = uint32(len(generationDomain) + 4 + 4 + 8*6 + 4)

// Manifest is the canonical logical membership authority for one projection.
// SetIDs are canonical vector-set/v1 checksums in strictly ascending order.
// Each member contributes every row of its source set, in source order. Row
// removal or replacement requires a new source set and a new manifest.
type Manifest struct {
	Checksum string
	SetIDs   []string
}

// Options bounds a build before it allocates or encodes a generation. Zero
// values select conservative defaults.
type Options struct {
	MaxRows      int
	MaxDimension int
	MaxBytes     int64
}

// RowIdentity remains stable across index-layout replacements.
type RowIdentity struct {
	SetID         string
	InputKey      string
	InputChecksum string
}

// Neighbor identifies a logical row without exposing its vector payload.
// Score is populated for cosine and dot product; Distance is populated for L2.
type Neighbor struct {
	RowIdentity

	Score    float64
	Distance float64
}

// Generation is a completely validated disposable vector-index generation.
type Generation struct {
	manifest      Manifest
	vectorSpaceID string
	metric        string
	normalization string
	dimension     int
	rows          []RowIdentity
	vectors       []float32
	sections      generationSections
}

// GenerationMetadata is the immutable validated identity and shape of an open
// projection. Metadata returns an independent copy of Manifest.SetIDs.
type GenerationMetadata struct {
	Format        string
	Layout        string
	Manifest      Manifest
	VectorSpaceID string
	Metric        string
	Normalization string
	Dimension     int
	RowCount      int
}

type buildProjection struct {
	space          string
	metric         string
	normalization  string
	dimension      int
	rowCount       int
	metadataLength uint64
	sections       generationSections
}

// NewManifest constructs sorted logical membership and its complete checksum.
func NewManifest(setIDs []string) (Manifest, error) {
	canonical := append([]string(nil), setIDs...)
	slices.Sort(canonical)
	manifest := Manifest{SetIDs: canonical}
	manifest.Checksum = manifestChecksum(canonical)
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// BuildGeneration builds one deterministic exact row-major projection. Vector
// sets may arrive in any order; manifest membership determines encoded order.
func BuildGeneration(manifest Manifest, sets []document.VectorSetV1, options Options) (*Generation, error) {
	bounds, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	if len(sets) != len(manifest.SetIDs) {
		return nil, errors.New("vector index sets do not match manifest membership")
	}
	projection, err := preflightBuildProjection(manifest, sets, bounds)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]document.VectorSetV1, len(sets))
	for index, set := range sets {
		_, id, encodeErr := document.EncodeVectorSetV1(set)
		if encodeErr != nil {
			return nil, fmt.Errorf("vector index source set %d: %w", index, encodeErr)
		}
		if _, duplicate := byID[id]; duplicate {
			return nil, fmt.Errorf("vector index source set %s has duplicate logical membership", id)
		}
		byID[id] = set
	}
	generation := &Generation{
		manifest:      Manifest{Checksum: manifest.Checksum, SetIDs: slices.Clone(manifest.SetIDs)},
		vectorSpaceID: projection.space, metric: projection.metric, normalization: projection.normalization,
		dimension: projection.dimension, sections: projection.sections,
		rows:    make([]RowIdentity, 0, projection.rowCount),
		vectors: make([]float32, projection.rowCount*projection.dimension),
	}
	for _, setID := range manifest.SetIDs {
		set, exists := byID[setID]
		if !exists {
			return nil, fmt.Errorf("vector index manifest names a missing vector set %s", setID)
		}
		for index, vector := range set.Vectors {
			copy(generation.vector(len(generation.rows)), vector)
			generation.rows = append(generation.rows, RowIdentity{
				SetID: setID, InputKey: set.InputKeys[index], InputChecksum: set.InputChecksums[index],
			})
		}
	}
	return generation, nil
}

func preflightBuildProjection(manifest Manifest, sets []document.VectorSetV1, bounds Options) (buildProjection, error) {
	projection := buildProjection{}
	for setIndex, set := range sets {
		if !validFingerprint(set.VectorSpaceFingerprint) || !document.IsValidVectorMetric(set.Metric) ||
			!validNormalization(set.Normalization) {
			return buildProjection{}, errors.New("vector index source set has an invalid descriptor")
		}
		if set.Dimension < 1 || set.Dimension > bounds.MaxDimension {
			return buildProjection{}, errors.New("vector index source allocation exceeds build bounds")
		}
		if len(set.Vectors) == 0 || len(set.InputKeys) != len(set.Vectors) ||
			len(set.InputChecksums) != len(set.Vectors) || len(set.Vectors) > bounds.MaxRows-projection.rowCount {
			return buildProjection{}, errors.New("vector index source allocation exceeds build bounds")
		}
		if setIndex == 0 {
			projection.space, projection.metric = set.VectorSpaceFingerprint, set.Metric
			projection.normalization, projection.dimension = set.Normalization, set.Dimension
		} else if set.VectorSpaceFingerprint != projection.space || set.Metric != projection.metric ||
			set.Normalization != projection.normalization || set.Dimension != projection.dimension {
			return buildProjection{}, errors.New("vector index source sets use incompatible vector spaces")
		}
		for rowIndex, vector := range set.Vectors {
			if len(vector) != set.Dimension {
				return buildProjection{}, errors.New("vector index source row dimension does not match descriptor")
			}
			if !validIdentity(set.InputKeys[rowIndex]) || !validFingerprint(set.InputChecksums[rowIndex]) {
				return buildProjection{}, errors.New("vector index source row has an invalid identity")
			}
			if err := validateMetricVector(set.Metric, set.Normalization, vector); err != nil {
				return buildProjection{}, err
			}
			if err := addFramedIdentityLength(&projection.metadataLength, sha256.Size*2); err != nil {
				return buildProjection{}, err
			}
			for _, value := range []string{set.InputKeys[rowIndex], set.InputChecksums[rowIndex]} {
				if err := addFramedStringLength(&projection.metadataLength, value); err != nil {
					return buildProjection{}, err
				}
			}
		}
		projection.rowCount += len(set.Vectors)
	}
	if projection.rowCount == 0 {
		return buildProjection{}, errors.New("vector index requires at least one logical row")
	}
	for _, value := range []string{generationLayout, manifest.Checksum, projection.space, projection.metric, projection.normalization} {
		if err := addFramedStringLength(&projection.metadataLength, value); err != nil {
			return buildProjection{}, err
		}
	}
	sections, err := computeSections(projection.metadataLength, projection.rowCount, projection.dimension, bounds.MaxBytes)
	if err != nil {
		return buildProjection{}, err
	}
	projection.sections = sections
	return projection, nil
}

// OpenGeneration reads and completely validates one exact generation before
// returning a searchable value.
func OpenGeneration(reader io.ReaderAt, size int64) (*Generation, error) {
	if reader == nil {
		return nil, errors.New("vector index reader is required")
	}
	if size < int64(generationHeaderSize)+generationChecksumBytes || size > defaultMaxBytes || size > int64(maxInt()) {
		return nil, errors.New("vector index generation size exceeds bounds")
	}
	var trailing [1]byte
	if count, err := reader.ReadAt(trailing[:], size); count != 0 || err == nil {
		return nil, errors.New("vector index generation has trailing bytes")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("probe vector index generation boundary: %w", err)
	}
	encoded := make([]byte, int(size))
	if _, err := io.ReadFull(io.NewSectionReader(reader, 0, size), encoded); err != nil {
		return nil, errors.New("vector index generation is truncated")
	}
	return decodeGeneration(encoded)
}

// Bytes deterministically encodes an independent v1 generation. Encoded bytes
// are not retained by Generation; callers may retain them for persistence.
// A nil or uninitialized generation returns nil.
func (generation *Generation) Bytes() []byte {
	if generation == nil || len(generation.rows) == 0 {
		return nil
	}
	return encodeGeneration(generation)
}

func (generation *Generation) vector(row int) []float32 {
	start := row * generation.dimension
	return generation.vectors[start : start+generation.dimension]
}

// Metadata returns the validated authority identity, descriptor, and shape
// required to fence a persisted generation and supply exact search bounds.
func (generation *Generation) Metadata() GenerationMetadata {
	if generation == nil {
		return GenerationMetadata{}
	}
	return GenerationMetadata{
		Format: generationFormat,
		Layout: generationLayout,
		Manifest: Manifest{
			Checksum: generation.manifest.Checksum,
			SetIDs:   slices.Clone(generation.manifest.SetIDs),
		},
		VectorSpaceID: generation.vectorSpaceID,
		Metric:        generation.metric, Normalization: generation.normalization,
		Dimension: generation.dimension, RowCount: len(generation.rows),
	}
}

// Search scans the complete generation and returns the exact k nearest rows.
func (generation *Generation) Search(query []float32, k int) ([]Neighbor, error) {
	if generation == nil || len(generation.rows) == 0 {
		return nil, errors.New("vector index generation is not open")
	}
	if k < 1 || k > len(generation.rows) {
		return nil, errors.New("vector index search k is outside row bounds")
	}
	if len(query) != generation.dimension {
		return nil, errors.New("vector index query dimension does not match generation")
	}
	if err := validateMetricVector(generation.metric, generation.normalization, query); err != nil {
		return nil, fmt.Errorf("vector index query: %w", err)
	}

	neighbors := make([]Neighbor, len(generation.rows))
	for index, row := range generation.rows {
		neighbor := Neighbor{RowIdentity: row}
		switch generation.metric {
		case document.VectorMetricCosine:
			neighbor.Score = cosine(query, generation.vector(index))
		case document.VectorMetricDotProduct:
			neighbor.Score = dot(query, generation.vector(index))
		case document.VectorMetricL2:
			neighbor.Distance = euclidean(query, generation.vector(index))
		}
		neighbors[index] = neighbor
	}
	sort.Slice(neighbors, func(left, right int) bool {
		if generation.metric == document.VectorMetricL2 {
			if neighbors[left].Distance != neighbors[right].Distance {
				return neighbors[left].Distance < neighbors[right].Distance
			}
		} else if neighbors[left].Score != neighbors[right].Score {
			return neighbors[left].Score > neighbors[right].Score
		}
		return compareIdentity(neighbors[left].RowIdentity, neighbors[right].RowIdentity) < 0
	})
	return append([]Neighbor(nil), neighbors[:k]...), nil
}

func normalizeOptions(options Options) (Options, error) {
	if options.MaxRows < 0 || options.MaxDimension < 0 || options.MaxBytes < 0 {
		return Options{}, errors.New("vector index build bounds cannot be negative")
	}
	if options.MaxRows == 0 {
		options.MaxRows = defaultMaxRows
	}
	if options.MaxDimension == 0 {
		options.MaxDimension = defaultMaxDimension
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultMaxBytes
	}
	if options.MaxRows > defaultMaxRows || options.MaxDimension > defaultMaxDimension || options.MaxBytes > defaultMaxBytes {
		return Options{}, errors.New("vector index build bounds exceed implementation limits")
	}
	return options, nil
}

// EffectiveOptions resolves zero-valued build bounds to the implementation
// defaults and rejects bounds outside the supported envelope.
func EffectiveOptions(options Options) (Options, error) {
	return normalizeOptions(options)
}

func validateManifest(manifest Manifest) error {
	if len(manifest.SetIDs) == 0 || len(manifest.SetIDs) > defaultMaxRows {
		return errors.New("vector index manifest membership exceeds bounds")
	}
	for index, setID := range manifest.SetIDs {
		if !validFingerprint(setID) {
			return errors.New("vector index manifest contains an invalid set identity")
		}
		if index > 0 && manifest.SetIDs[index-1] >= setID {
			return errors.New("vector index manifest is not canonically sorted")
		}
	}
	if !validFingerprint(manifest.Checksum) || manifest.Checksum != manifestChecksum(manifest.SetIDs) {
		return errors.New("vector index source manifest checksum mismatch")
	}
	return nil
}

func manifestChecksum(setIDs []string) string {
	hash := sha256.New()
	for _, setID := range setIDs {
		_, _ = hash.Write([]byte(setID))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// generationSections is shared by build sizing, encoding, and header validation.
type generationSections struct {
	metadataOffset uint64
	metadataLength uint64
	vectorOffset   uint64
	vectorLength   uint64
	checksumOffset uint64
	totalLength    uint64
}

func computeSections(metadataLength uint64, rows, dimension int, maxBytes int64) (generationSections, error) {
	if rows < 1 || rows > defaultMaxRows || dimension < 1 || dimension > defaultMaxDimension {
		return generationSections{}, errors.New("vector index generation dimensions exceed bounds")
	}
	if maxBytes < 1 || maxBytes > defaultMaxBytes {
		return generationSections{}, errors.New("vector index generation exceeds byte bounds")
	}
	vectorLength, ok := checkedProduct(uint64(rows), uint64(dimension), 4)
	vectorOffset, metadataOK := checkedAdd(uint64(generationHeaderSize), metadataLength)
	checksumOffset, vectorOK := checkedAdd(vectorOffset, vectorLength)
	totalLength, totalOK := checkedAdd(checksumOffset, generationChecksumBytes)
	if !ok || !metadataOK || !vectorOK || !totalOK || totalLength > uint64(maxBytes) {
		return generationSections{}, errors.New("vector index generation exceeds byte bounds")
	}
	return generationSections{uint64(generationHeaderSize), metadataLength, vectorOffset, vectorLength, checksumOffset, totalLength}, nil
}

func encodeGeneration(generation *Generation) []byte {
	sections := generation.sections
	output := make([]byte, 0, int(sections.totalLength)) //nolint:gosec // computeSections bounds total length to 512 MiB.
	output = append(output, generationDomain...)
	output = binary.LittleEndian.AppendUint32(output, generationVersion)
	output = binary.LittleEndian.AppendUint32(output, generationHeaderSize)
	for _, value := range []uint64{sections.metadataOffset, sections.metadataLength, sections.vectorOffset, sections.vectorLength, sections.checksumOffset, uint64(len(generation.rows))} {
		output = binary.LittleEndian.AppendUint64(output, value)
	}
	output = binary.LittleEndian.AppendUint32(output, uint32(generation.dimension)) //nolint:gosec // Validated dimension is at most 16,384.
	for _, value := range []string{generationLayout, generation.manifest.Checksum, generation.vectorSpaceID, generation.metric, generation.normalization} {
		output = appendString(output, value)
	}
	for _, row := range generation.rows {
		for _, value := range []string{row.SetID, row.InputKey, row.InputChecksum} {
			output = appendString(output, value)
		}
	}
	for _, value := range generation.vectors {
		if value == 0 {
			value = 0
		}
		output = binary.LittleEndian.AppendUint32(output, math.Float32bits(value))
	}
	checksum := generationChecksum(output)
	return append(output, checksum[:]...)
}

func appendString(output []byte, value string) []byte {
	output = binary.LittleEndian.AppendUint32(output, uint32(len(value))) //nolint:gosec // Validated strings are at most 64 KiB.
	return append(output, value...)
}

func addFramedStringLength(total *uint64, value string) error {
	if !validIdentity(value) {
		return errors.New("vector index identity must be bounded non-empty UTF-8")
	}
	return addFramedIdentityLength(total, len(value))
}

func addFramedIdentityLength(total *uint64, length int) error {
	if length < 1 || length > maxIdentityBytes {
		return errors.New("vector index identity length exceeds bounds")
	}
	framedLength, ok := checkedAdd(4, uint64(length))
	if !ok {
		return errors.New("vector index metadata length overflows")
	}
	updated, ok := checkedAdd(*total, framedLength)
	if !ok {
		return errors.New("vector index metadata length overflows")
	}
	*total = updated
	return nil
}

func decodeGeneration(encoded []byte) (*Generation, error) {
	sections, rows, dimension, err := decodeHeader(encoded)
	if err != nil {
		return nil, err
	}
	generation, err := decodeMetadata(encoded[sections.metadataOffset:sections.vectorOffset], rows, dimension)
	if err != nil {
		return nil, err
	}
	generation.sections = sections
	if err := generation.decodeVectors(encoded[sections.vectorOffset:sections.checksumOffset]); err != nil {
		return nil, err
	}
	if err := generation.verifySourceSetIdentities(); err != nil {
		return nil, err
	}
	return generation, nil
}

func decodeHeader(encoded []byte) (generationSections, int, int, error) {
	if len(encoded) < int(generationHeaderSize)+generationChecksumBytes {
		return generationSections{}, 0, 0, errors.New("vector index generation is truncated")
	}
	if string(encoded[:len(generationDomain)]) != generationDomain {
		return generationSections{}, 0, 0, errors.New("vector index generation has an invalid domain")
	}
	header := encoded[len(generationDomain):generationHeaderSize]
	if binary.LittleEndian.Uint32(header) != generationVersion {
		return generationSections{}, 0, 0, errors.New("vector index generation has an unsupported version")
	}
	if binary.LittleEndian.Uint32(header[4:]) != generationHeaderSize {
		return generationSections{}, 0, 0, errors.New("vector index generation has an invalid header size")
	}
	fields := header[8:]
	rowCount := binary.LittleEndian.Uint64(fields[40:])
	dimension := binary.LittleEndian.Uint32(fields[48:])
	if rowCount == 0 || rowCount > defaultMaxRows || dimension == 0 || dimension > defaultMaxDimension {
		return generationSections{}, 0, 0, errors.New("vector index generation dimensions exceed bounds")
	}
	metadataLength := binary.LittleEndian.Uint64(fields[8:])
	sections, err := computeSections(metadataLength, int(rowCount), int(dimension), defaultMaxBytes)
	if err != nil {
		return generationSections{}, 0, 0, err
	}
	if metadataLength == 0 || sections.metadataOffset != binary.LittleEndian.Uint64(fields) ||
		sections.vectorOffset != binary.LittleEndian.Uint64(fields[16:]) ||
		sections.vectorLength != binary.LittleEndian.Uint64(fields[24:]) ||
		sections.checksumOffset != binary.LittleEndian.Uint64(fields[32:]) || sections.totalLength != uint64(len(encoded)) {
		return generationSections{}, 0, 0, errors.New("vector index generation has invalid or overlapping sections")
	}
	checksum := generationChecksum(encoded[:sections.checksumOffset])
	if !bytes.Equal(checksum[:], encoded[sections.checksumOffset:]) {
		return generationSections{}, 0, 0, errors.New("vector index generation checksum mismatch")
	}
	return sections, int(rowCount), int(dimension), nil
}

func decodeMetadata(metadata []byte, rowCount, dimension int) (*Generation, error) {
	metadataReader := metadata
	layout, err := readString(&metadataReader)
	if err != nil || layout != generationLayout {
		return nil, errors.New("vector index generation has an unsupported layout")
	}
	manifestChecksumValue, err := readString(&metadataReader)
	if err != nil || !validFingerprint(manifestChecksumValue) {
		return nil, errors.New("vector index generation has an invalid manifest checksum")
	}
	space, err := readString(&metadataReader)
	if err != nil || !validFingerprint(space) {
		return nil, errors.New("vector index generation has an invalid vector-space identity")
	}
	metric, err := readString(&metadataReader)
	if err != nil || !document.IsValidVectorMetric(metric) {
		return nil, errors.New("vector index generation has an unsupported metric")
	}
	normalization, err := readString(&metadataReader)
	if err != nil || !validNormalization(normalization) {
		return nil, errors.New("vector index generation has an unsupported normalization")
	}

	rows := make([]RowIdentity, rowCount)
	setIDs := make([]string, 0)
	type logicalRowKey struct{ setID, inputKey string }
	seenRows := make(map[logicalRowKey]struct{}, rowCount)
	for index := range rows {
		setID, readErr := readString(&metadataReader)
		if readErr != nil || !validFingerprint(setID) {
			return nil, errors.New("vector index generation has an invalid row set identity")
		}
		inputKey, readErr := readString(&metadataReader)
		if readErr != nil || !validIdentity(inputKey) {
			return nil, errors.New("vector index generation has an invalid row input identity")
		}
		inputChecksum, readErr := readString(&metadataReader)
		if readErr != nil || !validFingerprint(inputChecksum) {
			return nil, errors.New("vector index generation has an invalid row checksum")
		}
		identity := RowIdentity{SetID: setID, InputKey: inputKey, InputChecksum: inputChecksum}
		logicalKey := logicalRowKey{setID: setID, inputKey: inputKey}
		if _, duplicate := seenRows[logicalKey]; duplicate {
			return nil, errors.New("vector index generation contains duplicate logical rows")
		}
		seenRows[logicalKey] = struct{}{}
		if len(setIDs) == 0 || setIDs[len(setIDs)-1] != setID {
			if len(setIDs) > 0 && setIDs[len(setIDs)-1] >= setID {
				return nil, errors.New("vector index generation rows are not canonically ordered")
			}
			setIDs = append(setIDs, setID)
		}
		rows[index] = identity
	}
	if len(metadataReader) != 0 {
		return nil, errors.New("vector index generation metadata has trailing bytes")
	}
	if manifestChecksum(setIDs) != manifestChecksumValue {
		return nil, errors.New("vector index generation manifest checksum does not match rows")
	}

	return &Generation{
		manifest:      Manifest{Checksum: manifestChecksumValue, SetIDs: setIDs},
		vectorSpaceID: space, metric: metric, normalization: normalization,
		dimension: dimension, rows: rows,
	}, nil
}

func (generation *Generation) decodeVectors(encoded []byte) error {
	generation.vectors = make([]float32, len(encoded)/4)
	for index := range generation.vectors {
		bits := binary.LittleEndian.Uint32(encoded[index*4:])
		value := math.Float32frombits(bits)
		if !finite(value) || value == 0 && bits != 0 {
			return fmt.Errorf("vector index row %d scalar %d: non-canonical vector scalars", index/generation.dimension, index%generation.dimension)
		}
		generation.vectors[index] = value
	}
	for index, row := range generation.rows {
		if err := validateMetricVector(generation.metric, generation.normalization, generation.vector(index)); err != nil {
			return fmt.Errorf("vector index set %s row %d (%q): %w", row.SetID, index, row.InputKey, err)
		}
	}
	return nil
}

func (generation *Generation) verifySourceSetIdentities() error {
	rows := generation.rows
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].SetID == rows[start].SetID {
			end++
		}
		set := document.VectorSetV1{
			VectorSpaceFingerprint: generation.vectorSpaceID,
			Metric:                 generation.metric,
			Normalization:          generation.normalization,
			Dimension:              generation.dimension,
			InputKeys:              make([]string, end-start),
			InputChecksums:         make([]string, end-start),
			Vectors:                make([][]float32, end-start),
		}
		for index := start; index < end; index++ {
			setIndex := index - start
			set.InputKeys[setIndex] = rows[index].InputKey
			set.InputChecksums[setIndex] = rows[index].InputChecksum
			set.Vectors[setIndex] = generation.vector(index)
		}
		_, sourceSetID, err := document.EncodeVectorSetV1(set)
		if err != nil || sourceSetID != rows[start].SetID {
			return fmt.Errorf("vector index generation does not match source set identity %s (rows %d-%d)", rows[start].SetID, start, end-1)
		}
		start = end
	}
	return nil
}

func validateMetricVector(metric, normalization string, vector []float32) error {
	if !document.IsValidVectorMetric(metric) || !validNormalization(normalization) {
		return errors.New("vector index metric or normalization is unsupported")
	}
	normSquared := 0.0
	for _, value := range vector {
		if !finite(value) {
			return errors.New("vector index contains a non-finite scalar")
		}
		normSquared += float64(value) * float64(value)
	}
	if metric == document.VectorMetricCosine && normSquared == 0 {
		return errors.New("vector index cosine vectors must be non-zero")
	}
	if normalization == document.VectorNormalizationUnitLength && math.Abs(math.Sqrt(normSquared)-1) > 1e-4 {
		return errors.New("vector index unit-length vector does not match normalization contract")
	}
	return nil
}

func validNormalization(normalization string) bool {
	return normalization == document.VectorNormalizationNone || normalization == document.VectorNormalizationUnitLength
}

func validFingerprint(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= maxIdentityBytes && utf8.ValidString(value)
}

func finite(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}

func dot(left, right []float32) float64 {
	result := 0.0
	for index := range left {
		result += float64(left[index]) * float64(right[index])
	}
	return result
}

func cosine(left, right []float32) float64 {
	dotProduct, leftNorm, rightNorm := 0.0, 0.0, 0.0
	for index := range left {
		leftValue, rightValue := float64(left[index]), float64(right[index])
		dotProduct += leftValue * rightValue
		leftNorm += leftValue * leftValue
		rightNorm += rightValue * rightValue
	}
	return dotProduct / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func euclidean(left, right []float32) float64 {
	squared := 0.0
	for index := range left {
		delta := float64(left[index]) - float64(right[index])
		squared += delta * delta
	}
	return math.Sqrt(squared)
}

func compareIdentity(left, right RowIdentity) int {
	if comparison := strings.Compare(left.SetID, right.SetID); comparison != 0 {
		return comparison
	}
	if comparison := strings.Compare(left.InputKey, right.InputKey); comparison != 0 {
		return comparison
	}
	return strings.Compare(left.InputChecksum, right.InputChecksum)
}

func generationChecksum(data []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(generationDomain))
	_, _ = hash.Write(data)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func readString(remaining *[]byte) (string, error) {
	data := *remaining
	if len(data) < 4 {
		return "", errors.New("vector index generation is truncated")
	}
	length := binary.LittleEndian.Uint32(data)
	data = data[4:]
	if length == 0 || length > maxIdentityBytes || uint64(length) > uint64(len(data)) {
		return "", errors.New("vector index generation has an invalid string length")
	}
	value := data[:length]
	if !utf8.Valid(value) {
		return "", errors.New("vector index generation contains invalid UTF-8")
	}
	*remaining = data[length:]
	return string(value), nil
}

func checkedAdd(left, right uint64) (uint64, bool) {
	if left > math.MaxUint64-right {
		return 0, false
	}
	return left + right, true
}

func checkedProduct(values ...uint64) (uint64, bool) {
	result := uint64(1)
	for _, value := range values {
		if value != 0 && result > math.MaxUint64/value {
			return 0, false
		}
		result *= value
	}
	return result, true
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
