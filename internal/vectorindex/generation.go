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

type generationRow struct {
	RowIdentity

	vector []float32
}

// Generation is a completely validated disposable vector-index generation.
type Generation struct {
	encoded       []byte
	manifest      Manifest
	vectorSpaceID string
	metric        string
	normalization string
	dimension     int
	rows          []generationRow
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
	vectorLength   uint64
	totalLength    uint64
}

// Builder incrementally validates and retains only the bounded row projection
// needed for one generation. It does not retain decoded vector-set payloads.
type Builder struct {
	manifest   Manifest
	bounds     Options
	projection buildProjection
	rows       []generationRow
	seen       map[string]struct{}
	ranks      map[string]int
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

// EffectiveOptions returns the exact conservative bounds used for a build.
func EffectiveOptions(options Options) (Options, error) {
	return normalizeOptions(options)
}

// NewBuilder starts a bounded incremental generation build.
func NewBuilder(manifest Manifest, options Options) (*Builder, error) {
	bounds, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	ranks := make(map[string]int, len(manifest.SetIDs))
	for index, setID := range manifest.SetIDs {
		ranks[setID] = index
	}
	return &Builder{manifest: manifest, bounds: bounds,
		seen: make(map[string]struct{}, len(manifest.SetIDs)), ranks: ranks}, nil
}

// Add validates one canonical set against cumulative generation bounds before
// copying its rows into bounded retained state.
func (builder *Builder) Add(setID string, set document.VectorSetV1) error {
	if builder == nil {
		return errors.New("vector index builder is required")
	}
	if _, member := builder.ranks[setID]; !member {
		return errors.New("vector index manifest names a missing vector set")
	}
	if _, duplicate := builder.seen[setID]; duplicate {
		return errors.New("vector index source sets contain duplicate logical membership")
	}
	_, canonicalID, err := document.EncodeVectorSetV1(set)
	if err != nil {
		return fmt.Errorf("vector index source set is invalid: %w", err)
	}
	if canonicalID != setID {
		return errors.New("vector index source set does not match manifest identity")
	}
	candidate := builder.projection
	if err := addSetToBuildProjection(&candidate, set, builder.bounds); err != nil {
		return err
	}
	if _, err := finishBuildProjection(builder.manifest, candidate, builder.bounds); err != nil {
		return err
	}
	for index, vector := range set.Vectors {
		builder.rows = append(builder.rows, generationRow{
			SetID: setID, InputKey: set.InputKeys[index], InputChecksum: set.InputChecksums[index], vector: append([]float32(nil), vector...)})
	}
	builder.projection = candidate
	builder.seen[setID] = struct{}{}
	return nil
}

// RowCount reports how much of the aggregate row budget is already retained.
func (builder *Builder) RowCount() int {
	if builder == nil {
		return 0
	}
	return builder.projection.rowCount
}

// Build encodes and reopens the completely validated deterministic generation.
func (builder *Builder) Build() (Generation, error) {
	if builder == nil {
		return Generation{}, errors.New("vector index builder is required")
	}
	if len(builder.seen) != len(builder.manifest.SetIDs) {
		return Generation{}, errors.New("vector index sets do not match manifest membership")
	}
	projection, err := finishBuildProjection(builder.manifest, builder.projection, builder.bounds)
	if err != nil {
		return Generation{}, err
	}
	sort.SliceStable(builder.rows, func(left, right int) bool {
		return builder.ranks[builder.rows[left].SetID] < builder.ranks[builder.rows[right].SetID]
	})
	encoded, err := encodeGeneration(builder.manifest, projection.space, projection.metric,
		projection.normalization, projection.dimension, builder.rows, builder.bounds.MaxBytes)
	if err != nil {
		return Generation{}, err
	}
	validated, err := OpenGeneration(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		return Generation{}, fmt.Errorf("validate built vector index generation: %w", err)
	}
	return *validated, nil
}

// BuildGeneration builds one deterministic exact row-major projection. Vector
// sets may arrive in any order; manifest membership determines encoded order.
func BuildGeneration(manifest Manifest, sets []document.VectorSetV1, options Options) (Generation, error) {
	if len(sets) != len(manifest.SetIDs) {
		return Generation{}, errors.New("vector index sets do not match manifest membership")
	}
	builder, err := NewBuilder(manifest, options)
	if err != nil {
		return Generation{}, err
	}
	for _, set := range sets {
		_, id, encodeErr := document.EncodeVectorSetV1(set)
		if encodeErr != nil {
			return Generation{}, fmt.Errorf("vector index source set is invalid: %w", encodeErr)
		}
		if err := builder.Add(id, set); err != nil {
			return Generation{}, err
		}
	}
	return builder.Build()
}

func addSetToBuildProjection(projection *buildProjection, set document.VectorSetV1, bounds Options) error {
	if !validFingerprint(set.VectorSpaceFingerprint) || !validMetric(set.Metric) ||
		!validNormalization(set.Normalization) {
		return errors.New("vector index source set has an invalid descriptor")
	}
	if set.Dimension < 1 || set.Dimension > bounds.MaxDimension {
		return errors.New("vector index source allocation exceeds build bounds")
	}
	if len(set.Vectors) == 0 || len(set.InputKeys) != len(set.Vectors) ||
		len(set.InputChecksums) != len(set.Vectors) || len(set.Vectors) > bounds.MaxRows-projection.rowCount {
		return errors.New("vector index source allocation exceeds build bounds")
	}
	if projection.rowCount == 0 {
		projection.space, projection.metric = set.VectorSpaceFingerprint, set.Metric
		projection.normalization, projection.dimension = set.Normalization, set.Dimension
	} else if set.VectorSpaceFingerprint != projection.space || set.Metric != projection.metric ||
		set.Normalization != projection.normalization || set.Dimension != projection.dimension {
		return errors.New("vector index source sets use incompatible vector spaces")
	}
	for rowIndex, vector := range set.Vectors {
		if len(vector) != set.Dimension {
			return errors.New("vector index source row dimension does not match descriptor")
		}
		if !validIdentity(set.InputKeys[rowIndex]) || !validFingerprint(set.InputChecksums[rowIndex]) {
			return errors.New("vector index source row has an invalid identity")
		}
		if err := validateMetricVector(set.Metric, set.Normalization, vector); err != nil {
			return err
		}
		if err := addFramedIdentityLength(&projection.metadataLength, sha256.Size*2); err != nil {
			return err
		}
		for _, value := range []string{set.InputKeys[rowIndex], set.InputChecksums[rowIndex]} {
			if err := addFramedStringLength(&projection.metadataLength, value); err != nil {
				return err
			}
		}
	}
	projection.rowCount += len(set.Vectors)
	return nil
}

func finishBuildProjection(manifest Manifest, projection buildProjection, bounds Options) (buildProjection, error) {
	if projection.rowCount == 0 {
		return buildProjection{}, errors.New("vector index requires at least one logical row")
	}
	for _, value := range []string{generationLayout, manifest.Checksum, projection.space, projection.metric, projection.normalization} {
		if err := addFramedStringLength(&projection.metadataLength, value); err != nil {
			return buildProjection{}, err
		}
	}
	var ok bool
	projection.vectorLength, ok = checkedProduct(
		uint64(projection.rowCount),  //nolint:gosec // row count is non-negative and bounded above.
		uint64(projection.dimension), //nolint:gosec // dimension is positive and bounded above.
		4,
	)
	if !ok {
		return buildProjection{}, errors.New("vector index scalar byte length overflows")
	}
	metadataEnd, metadataOK := checkedAdd(uint64(generationHeaderSize), projection.metadataLength)
	checksumOffset, vectorOK := checkedAdd(metadataEnd, projection.vectorLength)
	projection.totalLength, ok = checkedAdd(checksumOffset, generationChecksumBytes)
	if !metadataOK || !vectorOK || !ok || projection.totalLength > uint64(bounds.MaxBytes) { //nolint:gosec // normalized MaxBytes is positive.
		return buildProjection{}, errors.New("vector index generation exceeds byte bounds")
	}
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

// Bytes returns an independent copy of the deterministic generation bytes.
func (generation *Generation) Bytes() []byte {
	return append([]byte(nil), generation.encoded...)
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

// Search performs exact search. maxVisits must cover the full generation so
// callers cannot accidentally mistake a truncated scan for exact results.
func (generation *Generation) Search(query []float32, k, maxVisits int) ([]Neighbor, error) {
	if generation == nil {
		return nil, errors.New("vector index generation is not open")
	}
	return generation.searchRows(query, generation.rows, k, maxVisits)
}

// SearchRows performs exact search over exactly the supplied logical rows.
// Every supplied identity must occur once in this generation, so callers can
// fence the ANN input before vector scoring begins.
func (generation *Generation) SearchRows(query []float32, identities []RowIdentity) ([]Neighbor, error) {
	if generation == nil || len(generation.rows) == 0 {
		return nil, errors.New("vector index generation is not open")
	}
	if len(identities) == 0 {
		return nil, nil
	}
	selected := make(map[RowIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if _, duplicate := selected[identity]; duplicate {
			return nil, errors.New("vector index selected rows contain a duplicate identity")
		}
		selected[identity] = struct{}{}
	}
	rows := make([]generationRow, 0, len(identities))
	for _, row := range generation.rows {
		if _, included := selected[row.RowIdentity]; included {
			rows = append(rows, row)
		}
	}
	if len(rows) != len(selected) {
		return nil, errors.New("vector index selected row is absent from generation")
	}
	return generation.searchRows(query, rows, len(rows), len(rows))
}

func (generation *Generation) searchRows(query []float32, rows []generationRow, k, maxVisits int) ([]Neighbor, error) {
	if len(rows) == 0 {
		return nil, errors.New("vector index generation is not open")
	}
	if k < 1 || k > len(rows) {
		return nil, errors.New("vector index search k is outside row bounds")
	}
	if maxVisits != len(rows) || maxVisits < k {
		return nil, errors.New("vector index exact search visit bound must equal row count")
	}
	if len(query) != generation.dimension {
		return nil, errors.New("vector index query dimension does not match generation")
	}
	if err := validateMetricVector(generation.metric, generation.normalization, query); err != nil {
		return nil, fmt.Errorf("vector index query: %w", err)
	}

	neighbors := make([]Neighbor, len(rows))
	for index, row := range rows {
		neighbor := Neighbor{RowIdentity: row.RowIdentity}
		switch generation.metric {
		case document.VectorMetricCosine:
			neighbor.Score = cosine(query, row.vector)
		case document.VectorMetricDotProduct:
			neighbor.Score = dot(query, row.vector)
		case document.VectorMetricL2:
			neighbor.Distance = euclidean(query, row.vector)
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

func encodeGeneration(manifest Manifest, space, metric, normalization string, dimension int, rows []generationRow, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 || maxBytes > defaultMaxBytes {
		return nil, errors.New("vector index generation exceeds byte bounds")
	}
	metadataLength := uint64(0)
	for _, value := range []string{generationLayout, manifest.Checksum, space, metric, normalization} {
		if err := addFramedStringLength(&metadataLength, value); err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		if len(row.vector) != dimension {
			return nil, errors.New("vector index row dimension does not match generation")
		}
		for _, value := range []string{row.SetID, row.InputKey, row.InputChecksum} {
			if err := addFramedStringLength(&metadataLength, value); err != nil {
				return nil, err
			}
		}
	}
	vectorLength, ok := checkedProduct(
		uint64(len(rows)),
		uint64(dimension), //nolint:gosec // dimension is positive and bounded above.
		4,
	)
	if !ok {
		return nil, errors.New("vector index scalar byte length overflows")
	}
	metadataOffset := uint64(generationHeaderSize)
	vectorOffset, ok := checkedAdd(metadataOffset, metadataLength)
	if !ok {
		return nil, errors.New("vector index metadata offset overflows")
	}
	checksumOffset, ok := checkedAdd(vectorOffset, vectorLength)
	totalLength, totalOK := checkedAdd(checksumOffset, generationChecksumBytes)
	if !ok || !totalOK || totalLength > uint64(maxBytes) {
		return nil, errors.New("vector index generation exceeds byte bounds")
	}

	var metadata bytes.Buffer
	metadata.Grow(int(metadataLength))
	for _, value := range []string{generationLayout, manifest.Checksum, space, metric, normalization} {
		if err := writeString(&metadata, value); err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		for _, value := range []string{row.SetID, row.InputKey, row.InputChecksum} {
			if err := writeString(&metadata, value); err != nil {
				return nil, err
			}
		}
	}
	var output bytes.Buffer
	output.Grow(int(totalLength)) //nolint:gosec // total length is bounded by 512 MiB above.
	output.WriteString(generationDomain)
	writeUint32(&output, generationVersion)
	writeUint32(&output, generationHeaderSize)
	for _, value := range []uint64{metadataOffset, metadataLength, vectorOffset, vectorLength, checksumOffset, uint64(len(rows))} {
		writeUint64(&output, value)
	}
	writeUint32(&output, uint32(dimension)) //nolint:gosec // dimension is bounded by 16,384 above.
	output.Write(metadata.Bytes())
	for _, row := range rows {
		for _, value := range row.vector {
			if value == 0 {
				value = 0
			}
			writeUint32(&output, math.Float32bits(value))
		}
	}
	checksum := generationChecksum(output.Bytes())
	output.Write(checksum[:])
	return output.Bytes(), nil
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
	if len(encoded) < int(generationHeaderSize)+generationChecksumBytes {
		return nil, errors.New("vector index generation is truncated")
	}
	reader := bytes.NewReader(encoded)
	magic := make([]byte, len(generationDomain))
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != generationDomain {
		return nil, errors.New("vector index generation has an invalid domain")
	}
	version, err := readUint32(reader)
	if err != nil || version != generationVersion {
		return nil, errors.New("vector index generation has an unsupported version")
	}
	headerSize, err := readUint32(reader)
	if err != nil || headerSize != generationHeaderSize {
		return nil, errors.New("vector index generation has an invalid header size")
	}
	fields := make([]uint64, 6)
	for index := range fields {
		fields[index], err = readUint64(reader)
		if err != nil {
			return nil, err
		}
	}
	dimension32, err := readUint32(reader)
	if err != nil {
		return nil, err
	}
	metadataOffset, metadataLength, vectorOffset, vectorLength, checksumOffset, rowCount := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]
	if rowCount == 0 || rowCount > defaultMaxRows || dimension32 == 0 || dimension32 > defaultMaxDimension {
		return nil, errors.New("vector index generation dimensions exceed bounds")
	}
	expectedVectorLength, ok := checkedProduct(rowCount, uint64(dimension32), 4)
	metadataEnd, metadataOK := checkedAdd(metadataOffset, metadataLength)
	vectorEnd, vectorOK := checkedAdd(vectorOffset, vectorLength)
	fileEnd, fileOK := checkedAdd(checksumOffset, generationChecksumBytes)
	if !ok || !metadataOK || !vectorOK || !fileOK || metadataOffset != uint64(headerSize) ||
		metadataLength == 0 || metadataEnd != vectorOffset || expectedVectorLength != vectorLength ||
		vectorEnd != checksumOffset || fileEnd != uint64(len(encoded)) {
		return nil, errors.New("vector index generation has invalid or overlapping sections")
	}
	wantChecksum := generationChecksum(encoded[:checksumOffset])
	if !bytes.Equal(wantChecksum[:], encoded[checksumOffset:]) {
		return nil, errors.New("vector index generation checksum mismatch")
	}

	metadataReader := bytes.NewReader(encoded[metadataOffset:metadataEnd])
	layout, err := readString(metadataReader)
	if err != nil || layout != generationLayout {
		return nil, errors.New("vector index generation has an unsupported layout")
	}
	manifestChecksumValue, err := readString(metadataReader)
	if err != nil || !validFingerprint(manifestChecksumValue) {
		return nil, errors.New("vector index generation has an invalid manifest checksum")
	}
	space, err := readString(metadataReader)
	if err != nil || !validFingerprint(space) {
		return nil, errors.New("vector index generation has an invalid vector-space identity")
	}
	metric, err := readString(metadataReader)
	if err != nil || !validMetric(metric) {
		return nil, errors.New("vector index generation has an unsupported metric")
	}
	normalization, err := readString(metadataReader)
	if err != nil || !validNormalization(normalization) {
		return nil, errors.New("vector index generation has an unsupported normalization")
	}

	rows := make([]generationRow, int(rowCount))
	setIDs := make([]string, 0)
	type logicalRowKey struct{ setID, inputKey string }
	seenRows := make(map[logicalRowKey]struct{}, int(rowCount))
	for index := range rows {
		setID, readErr := readString(metadataReader)
		if readErr != nil || !validFingerprint(setID) {
			return nil, errors.New("vector index generation has an invalid row set identity")
		}
		inputKey, readErr := readString(metadataReader)
		if readErr != nil || !validIdentity(inputKey) {
			return nil, errors.New("vector index generation has an invalid row input identity")
		}
		inputChecksum, readErr := readString(metadataReader)
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
		rows[index].RowIdentity = identity
	}
	if metadataReader.Len() != 0 {
		return nil, errors.New("vector index generation metadata has trailing bytes")
	}
	if manifestChecksum(setIDs) != manifestChecksumValue {
		return nil, errors.New("vector index generation manifest checksum does not match rows")
	}

	vectorReader := bytes.NewReader(encoded[vectorOffset:vectorEnd])
	for index := range rows {
		rows[index].vector = make([]float32, int(dimension32))
		for scalar := range rows[index].vector {
			bits, readErr := readUint32(vectorReader)
			if readErr != nil {
				return nil, readErr
			}
			value := math.Float32frombits(bits)
			if !finite(value) || value == 0 && bits != 0 {
				return nil, errors.New("vector index generation contains non-canonical vector scalars")
			}
			rows[index].vector[scalar] = value
		}
		if err := validateMetricVector(metric, normalization, rows[index].vector); err != nil {
			return nil, err
		}
	}
	if vectorReader.Len() != 0 {
		return nil, errors.New("vector index generation vector payload has trailing bytes")
	}
	if err := verifySourceSetIdentities(rows, space, metric, normalization, int(dimension32)); err != nil {
		return nil, err
	}

	return &Generation{
		encoded: encoded, manifest: Manifest{Checksum: manifestChecksumValue, SetIDs: setIDs},
		vectorSpaceID: space, metric: metric, normalization: normalization,
		dimension: int(dimension32), rows: rows,
	}, nil
}

func verifySourceSetIdentities(rows []generationRow, space, metric, normalization string, dimension int) error {
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].SetID == rows[start].SetID {
			end++
		}
		set := document.VectorSetV1{
			VectorSpaceFingerprint: space,
			Metric:                 metric,
			Normalization:          normalization,
			Dimension:              dimension,
			InputKeys:              make([]string, end-start),
			InputChecksums:         make([]string, end-start),
			Vectors:                make([][]float32, end-start),
		}
		for index := start; index < end; index++ {
			setIndex := index - start
			set.InputKeys[setIndex] = rows[index].InputKey
			set.InputChecksums[setIndex] = rows[index].InputChecksum
			set.Vectors[setIndex] = rows[index].vector
		}
		_, sourceSetID, err := document.EncodeVectorSetV1(set)
		if err != nil || sourceSetID != rows[start].SetID {
			return errors.New("vector index generation does not match source set identity")
		}
		start = end
	}
	return nil
}

func validateMetricVector(metric, normalization string, vector []float32) error {
	if !validMetric(metric) || !validNormalization(normalization) {
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

func validMetric(metric string) bool {
	return metric == document.VectorMetricCosine || metric == document.VectorMetricDotProduct || metric == document.VectorMetricL2
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

func writeString(output *bytes.Buffer, value string) error {
	if !validIdentity(value) {
		return errors.New("vector index identity must be bounded non-empty UTF-8")
	}
	writeUint32(output, uint32(len(value))) //nolint:gosec // validIdentity bounds length to 64 KiB.
	output.WriteString(value)
	return nil
}

func readString(reader *bytes.Reader) (string, error) {
	length, err := readUint32(reader)
	if err != nil {
		return "", err
	}
	if length == 0 || length > maxIdentityBytes || uint64(length) > uint64(reader.Len()) { //nolint:gosec // bytes.Reader length cannot be negative.
		return "", errors.New("vector index generation has an invalid string length")
	}
	value := make([]byte, int(length))
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", errors.New("vector index generation is truncated")
	}
	if !utf8.Valid(value) {
		return "", errors.New("vector index generation contains invalid UTF-8")
	}
	return string(value), nil
}

func writeUint32(output *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], value)
	output.Write(encoded[:])
}

func writeUint64(output *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	output.Write(encoded[:])
}

func readUint32(reader *bytes.Reader) (uint32, error) {
	var encoded [4]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, errors.New("vector index generation is truncated")
	}
	return binary.LittleEndian.Uint32(encoded[:]), nil
}

func readUint64(reader *bytes.Reader) (uint64, error) {
	var encoded [8]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, errors.New("vector index generation is truncated")
	}
	return binary.LittleEndian.Uint64(encoded[:]), nil
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
