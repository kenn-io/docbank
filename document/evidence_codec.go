package document

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/internal/manifestjson"
	"golang.org/x/text/unicode/norm"
)

const (
	maxEvidenceIdentifierBytes  = 1 << 10
	maxEvidencePointerBytes     = 1 << 10
	maxEvidenceReasonBytes      = 1 << 12
	maxEvidenceTextBytes        = 256 << 20
	maxEvidenceCoordinate       = int64(1_000_000_000_000_000)
	maxEvidenceGeometryBoxes    = 10_000
	maxEvidenceGeometryPolygons = 10_000
	maxEvidenceGeometryPoints   = 100_000
)

// ValidateSourceEvidenceV1 validates a bounded source-evidence/v1 manifest
// without assigning durable IDs.
func ValidateSourceEvidenceV1(source SourceEvidenceV1) error {
	_, err := validateSourceEvidenceV1(source)
	return err
}

func validateSourceEvidenceV1(source SourceEvidenceV1) ([]evidenceTextMap, error) {
	if source.ContractVersion != SourceEvidenceContractV1 {
		return nil, fmt.Errorf("source evidence contract version must be %q", SourceEvidenceContractV1)
	}
	if !validEvidenceCompleteness(source.Completeness) {
		return nil, errors.New("source evidence has invalid completeness")
	}
	if err := validateEvidenceIdentifier(source.Family, "document family"); err != nil {
		return nil, err
	}
	if !validEvidenceUnitKind(source.UnitKind) {
		return nil, errors.New("source evidence has invalid unit kind")
	}
	if len(source.Units) == 0 || len(source.Units) > 100_000 {
		return nil, errors.New("source evidence must contain a bounded non-empty unit list")
	}
	if err := validateCompletenessOmissions(source); err != nil {
		return nil, err
	}

	artifactIDs, err := validateSourceArtifacts(source.Artifacts)
	if err != nil {
		return nil, err
	}
	textMaps := make([]evidenceTextMap, len(source.Units))
	documentRangeOffsets := partitionSourceOmissionRangeOffsets(source.Omissions, len(source.Units))
	for index, unit := range source.Units {
		if err := validateEvidenceText(unit.Text, "unit text"); err != nil {
			return nil, fmt.Errorf("source evidence unit %d: %w", index, err)
		}
		textMaps[index] = newEvidenceTextMap(unit.Text, collectSourceRangeOffsets(unit, documentRangeOffsets[index]))
	}
	for index := range source.Units {
		if err := validateSourceUnit(source, index, artifactIDs, textMaps[index]); err != nil {
			return nil, err
		}
	}
	if err := validateSourceOmissions(source.Omissions, textMaps); err != nil {
		return nil, fmt.Errorf("source evidence omissions: %w", err)
	}
	return textMaps, nil
}

func validateSourceArtifacts(artifacts []SourceEvidenceArtifactV1) (map[string]struct{}, error) {
	if len(artifacts) > 10_000 {
		return nil, errors.New("source evidence has too many artifacts")
	}
	providerIDs := make(map[string]struct{}, len(artifacts))
	canonicalArtifacts := make(map[struct {
		pointer string
		role    EvidenceArtifactRole
		sha256  string
	}]struct{}, len(artifacts))
	for index, artifact := range artifacts {
		if err := validateProviderID(artifact.ProviderID, providerIDs); err != nil {
			return nil, fmt.Errorf("source evidence artifact %d: %w", index, err)
		}
		if !validEvidenceArtifactRole(artifact.Role) {
			return nil, fmt.Errorf("source evidence artifact %d has invalid role", index)
		}
		if len(artifact.SHA256) != sha256.Size*2 || !manifestjson.LowerHex(artifact.SHA256) {
			return nil, fmt.Errorf("source evidence artifact %d has invalid SHA-256", index)
		}
		if err := validateArtifactPointer(artifact.Pointer); err != nil {
			return nil, fmt.Errorf("source evidence artifact %d: %w", index, err)
		}
		canonicalIdentity := struct {
			pointer string
			role    EvidenceArtifactRole
			sha256  string
		}{canonicalEvidenceString(artifact.Pointer), artifact.Role, artifact.SHA256}
		if _, exists := canonicalArtifacts[canonicalIdentity]; exists {
			return nil, fmt.Errorf("source evidence artifact %d duplicates a canonical identity", index)
		}
		canonicalArtifacts[canonicalIdentity] = struct{}{}
	}
	return providerIDs, nil
}

func validateSourceUnit(
	source SourceEvidenceV1,
	index int,
	artifactIDs map[string]struct{},
	textMap evidenceTextMap,
) error {
	unit := source.Units[index]
	if unit.Order != index {
		return fmt.Errorf("source evidence has noncontiguous unit order at %d: got %d", index, unit.Order)
	}
	if unit.ProviderID != "" {
		if err := validateBoundedUTF8(unit.ProviderID, maxEvidenceIdentifierBytes, "unit provider ID"); err != nil {
			return fmt.Errorf("source evidence unit %d: %w", index, err)
		}
	}
	if err := validateSourceLocator(source.Family, source.UnitKind, source.Completeness, unit.Locator); err != nil {
		return fmt.Errorf("source evidence unit %d: %w", index, err)
	}
	for headingIndex, heading := range unit.HeadingPath {
		if err := validateEvidenceText(heading, "heading"); err != nil || heading == "" {
			if err == nil {
				err = errors.New("heading is empty")
			}
			return fmt.Errorf("source evidence unit %d heading %d: %w", index, headingIndex, err)
		}
	}
	if err := validateSourceConfidence(unit.Confidence); err != nil {
		return fmt.Errorf("source evidence unit %d: %w", index, err)
	}
	regionIDs, err := validateSourceRegions(index, textMap, unit.Regions, artifactIDs)
	if err != nil {
		return err
	}
	if err := validateSourceTables(index, textMap, unit.Tables, regionIDs); err != nil {
		return err
	}
	if err := validateUnitOmissions(unit.Omissions, index, textMap); err != nil {
		return fmt.Errorf("source evidence unit %d omissions: %w", index, err)
	}
	return nil
}

func validateSourceLocator(
	family string,
	unitKind EvidenceUnitKind,
	completeness EvidenceCompleteness,
	locator SourceEvidenceLocatorV1,
) error {
	want := locatorKindForUnit(unitKind)
	if locator.Kind != want {
		return fmt.Errorf("%s locator is required for unit kind %q", want, unitKind)
	}
	if err := validateFamilyUnitKindForCompleteness(family, unitKind, completeness); err != nil {
		return err
	}
	if locator.Name != "" {
		if err := validateBoundedUTF8(locator.Name, maxEvidenceIdentifierBytes, "locator name"); err != nil {
			return err
		}
	}
	if locator.Kind == EvidenceLocatorTime {
		if locator.IndexOrigin != EvidenceIndexOriginNone || locator.Start < 0 || locator.End <= locator.Start {
			return errors.New("time-range locator must contain a positive half-open millisecond range")
		}
		return nil
	}
	if locator.Kind == EvidenceLocatorGeneric || locator.Kind == EvidenceLocatorMessage ||
		locator.Kind == EvidenceLocatorSection {
		if locator.IndexOrigin != EvidenceIndexOriginNone || locator.Start != 0 || locator.End != 0 {
			return fmt.Errorf("%s locator must not claim an index", locator.Kind)
		}
		return nil
	}
	if locator.IndexOrigin != EvidenceIndexOriginZero && locator.IndexOrigin != EvidenceIndexOriginOne {
		return fmt.Errorf("%s locator must declare zero- or one-based indexing", locator.Kind)
	}
	minimum := int64(0)
	if locator.IndexOrigin == EvidenceIndexOriginOne {
		minimum = 1
	}
	if locator.Start < minimum || locator.End < locator.Start || locator.End > maxEvidenceCoordinate {
		return fmt.Errorf("%s locator has an invalid range", locator.Kind)
	}
	if locator.Kind == EvidenceLocatorPage || locator.Kind == EvidenceLocatorSlide ||
		locator.Kind == EvidenceLocatorSheet || locator.Kind == EvidenceLocatorSpine {
		if locator.End != locator.Start {
			return fmt.Errorf("%s locator must identify one ordered unit", locator.Kind)
		}
	}
	if locator.Kind == EvidenceLocatorSheet && strings.TrimSpace(locator.Name) == "" {
		return errors.New("sheet locator requires a stable name")
	}
	return nil
}

func validateFamilyUnitKindForCompleteness(
	family string,
	unitKind EvidenceUnitKind,
	completeness EvidenceCompleteness,
) error {
	if completeness == EvidenceDegradedProvenance {
		if unitKind != EvidenceUnitGeneric {
			return errors.New("degraded-provenance evidence must use generic units")
		}
		return nil
	}
	if unitKind == EvidenceUnitGeneric {
		return errors.New("generic units must be marked degraded-provenance")
	}
	return validateFamilyUnitKind(family, unitKind)
}

func validateFamilyUnitKind(family string, unitKind EvidenceUnitKind) error {
	var allowed bool
	switch family {
	case "pdf", "word":
		allowed = unitKind == EvidenceUnitPage
	case "presentation":
		allowed = unitKind == EvidenceUnitSlide
	case "spreadsheet":
		allowed = unitKind == EvidenceUnitSheet || unitKind == EvidenceUnitRecord
	case "ebook":
		allowed = unitKind == EvidenceUnitSpine
	case "structured":
		allowed = unitKind == EvidenceUnitRecord
	case "source", "text":
		allowed = unitKind == EvidenceUnitSection || unitKind == EvidenceUnitLine
	case "mail":
		allowed = unitKind == EvidenceUnitMessage
	default:
		return fmt.Errorf("unknown document family %q", family)
	}
	if !allowed {
		return fmt.Errorf("document family %q cannot use unit kind %q", family, unitKind)
	}
	return nil
}

func validateSourceRegions(
	unitIndex int,
	textMap evidenceTextMap,
	regions []SourceEvidenceRegionV1,
	artifactIDs map[string]struct{},
) (map[string]sourceRegionRef, error) {
	if len(regions) > 1_000_000 {
		return nil, fmt.Errorf("source evidence unit %d has too many regions", unitIndex)
	}
	providerIDs := make(map[string]sourceRegionRef, len(regions))
	for index, region := range regions {
		if region.Order != index {
			return nil, fmt.Errorf("source evidence unit %d has noncontiguous region order", unitIndex)
		}
		if err := validateSourceRegionID(region.ProviderID, region.Kind, providerIDs, index); err != nil {
			return nil, fmt.Errorf("source evidence unit %d region %d: %w", unitIndex, index, err)
		}
		if !validEvidenceRegionKind(region.Kind) {
			return nil, fmt.Errorf("source evidence unit %d region %d has invalid kind", unitIndex, index)
		}
		if region.ParentProviderID != "" {
			parent, ok := providerIDs[region.ParentProviderID]
			if !ok || parent.order >= index {
				return nil, fmt.Errorf("source evidence unit %d region %d has unknown parent", unitIndex, index)
			}
		}
		if region.ArtifactProviderID != "" {
			if _, ok := artifactIDs[region.ArtifactProviderID]; !ok {
				return nil, fmt.Errorf("source evidence unit %d region %d has unknown artifact", unitIndex, index)
			}
		}
		if _, err := textMap.normalizeRange(region.TextRange); err != nil {
			return nil, fmt.Errorf("source evidence unit %d region %d text range: %w", unitIndex, index, err)
		}
		if err := validateSourceConfidence(region.Confidence); err != nil {
			return nil, fmt.Errorf("source evidence unit %d region %d: %w", unitIndex, index, err)
		}
		if err := validateSourceGeometry(region.Geometry); err != nil {
			return nil, fmt.Errorf("source evidence unit %d region %d: %w", unitIndex, index, err)
		}
	}
	return providerIDs, nil
}

func validateSourceTables(
	unitIndex int,
	textMap evidenceTextMap,
	tables []SourceEvidenceTableV1,
	regionIDs map[string]sourceRegionRef,
) error {
	if len(tables) > 100_000 {
		return fmt.Errorf("source evidence unit %d has too many tables", unitIndex)
	}
	providerIDs := make(map[string]struct{}, len(tables))
	for index, table := range tables {
		if table.Order != index {
			return fmt.Errorf("source evidence unit %d has noncontiguous table order", unitIndex)
		}
		if err := validateProviderID(table.ProviderID, providerIDs); err != nil {
			return fmt.Errorf("source evidence unit %d table %d: %w", unitIndex, index, err)
		}
		if table.Rows <= 0 || table.Columns <= 0 || table.Rows > 1_000_000 || table.Columns > 1_000_000 {
			return fmt.Errorf("source evidence unit %d table %d has invalid dimensions", unitIndex, index)
		}
		if table.RegionProviderID != "" {
			region, ok := regionIDs[table.RegionProviderID]
			if !ok {
				return fmt.Errorf("source evidence unit %d table %d has unknown region", unitIndex, index)
			}
			if region.kind != EvidenceRegionTable {
				return fmt.Errorf("source evidence unit %d table %d has invalid table region", unitIndex, index)
			}
		}
		if len(table.Cells) > 1_000_000 {
			return fmt.Errorf("source evidence unit %d table %d has too many cells", unitIndex, index)
		}
		for cellIndex, cell := range table.Cells {
			if err := validateSourceTableCell(textMap, table, cell, cellIndex, regionIDs); err != nil {
				return fmt.Errorf("source evidence unit %d table %d cell %d: %w", unitIndex, index, cellIndex, err)
			}
		}
	}
	return nil
}

func validateSourceTableCell(
	textMap evidenceTextMap,
	table SourceEvidenceTableV1,
	cell SourceEvidenceTableCellV1,
	index int,
	regionIDs map[string]sourceRegionRef,
) error {
	if cell.Order != index {
		return errors.New("noncontiguous cell order")
	}
	if cell.Row < 0 || cell.Column < 0 || cell.RowSpan <= 0 || cell.ColumnSpan <= 0 ||
		cell.RowSpan > table.Rows || cell.ColumnSpan > table.Columns ||
		cell.Row > table.Rows-cell.RowSpan || cell.Column > table.Columns-cell.ColumnSpan {
		return errors.New("cell coordinates exceed table dimensions")
	}
	if cell.RegionProviderID != "" {
		region, ok := regionIDs[cell.RegionProviderID]
		if !ok {
			return errors.New("unknown region")
		}
		if region.kind != EvidenceRegionTableCell {
			return errors.New("invalid cell region")
		}
	}
	if _, err := textMap.normalizeRange(cell.TextRange); err != nil {
		return fmt.Errorf("text range: %w", err)
	}
	return nil
}

func validateSourceConfidence(confidence *SourceEvidenceConfidenceV1) error {
	if confidence == nil {
		return nil
	}
	if !validEvidenceConfidenceInterpretation(confidence.Interpretation) ||
		!finite(confidence.Minimum) || !finite(confidence.Maximum) || !finite(confidence.Value) ||
		confidence.Minimum >= confidence.Maximum || confidence.Value < confidence.Minimum ||
		confidence.Value > confidence.Maximum || math.Abs(confidence.Minimum) > 1_000_000 ||
		math.Abs(confidence.Maximum) > 1_000_000 {
		return errors.New("source evidence confidence is invalid")
	}
	if confidence.Interpretation == EvidenceConfidenceProbability &&
		(confidence.Minimum != 0 || confidence.Maximum != 1) {
		return errors.New("source evidence probability confidence must use the [0,1] scale")
	}
	normalized := normalizeConfidence(confidence)
	if normalized.Minimum >= normalized.Maximum || normalized.Value < normalized.Minimum ||
		normalized.Value > normalized.Maximum {
		return errors.New("source evidence fixed-point confidence is invalid")
	}
	return nil
}

func validateSourceGeometry(geometry *SourceEvidenceGeometryV1) error {
	if geometry == nil {
		return nil
	}
	if !validCoordinateOrigin(geometry.CoordinateOrigin) || !validCoordinateSpace(geometry.CoordinateSpace) ||
		!validGeometryUnit(geometry.Unit) || geometry.Scale <= 0 || geometry.Scale > 1_000_000_000 ||
		geometry.Width <= 0 || geometry.Height <= 0 || geometry.Width > maxEvidenceCoordinate ||
		geometry.Height > maxEvidenceCoordinate || abs64(geometry.Orientation) > 360*geometry.Scale {
		return errors.New("source evidence geometry frame is invalid")
	}
	if len(geometry.Boxes) > maxEvidenceGeometryBoxes {
		return errors.New("source evidence geometry has too many boxes")
	}
	if len(geometry.Polygons) > maxEvidenceGeometryPolygons {
		return errors.New("source evidence geometry has too many polygons")
	}
	for index, box := range geometry.Boxes {
		if err := validateEvidenceBox(geometry, box); err != nil {
			return fmt.Errorf("source evidence geometry box %d: %w", index, err)
		}
	}
	remainingPoints := maxEvidenceGeometryPoints
	for index, polygon := range geometry.Polygons {
		if len(polygon.Points) < 3 {
			return fmt.Errorf("source evidence geometry polygon %d has invalid point count", index)
		}
		if len(polygon.Points) > remainingPoints {
			return errors.New("source evidence geometry has too many polygon points")
		}
		remainingPoints -= len(polygon.Points)
		for _, point := range polygon.Points {
			if point.X < 0 || point.Y < 0 || point.X > geometry.Width || point.Y > geometry.Height {
				return fmt.Errorf("source evidence geometry polygon %d leaves its frame", index)
			}
		}
	}
	return nil
}

func validateEvidenceBox(geometry *SourceEvidenceGeometryV1, box EvidenceBoxV1) error {
	if box.Left < 0 || box.Right <= box.Left || box.Right > geometry.Width || box.Top < 0 || box.Bottom < 0 ||
		box.Top > geometry.Height || box.Bottom > geometry.Height {
		return errors.New("coordinates leave their frame")
	}
	if geometry.CoordinateOrigin == EvidenceCoordinateTopLeft && box.Bottom <= box.Top {
		return errors.New("bottom must follow top for top-left coordinates")
	}
	if geometry.CoordinateOrigin == EvidenceCoordinateBottomLeft && box.Top <= box.Bottom {
		return errors.New("top must follow bottom for bottom-left coordinates")
	}
	return nil
}

// NormalizeEvidenceV1 validates and canonicalizes source evidence, replacing
// provider-local IDs with deterministic generation-local IDs.
func NormalizeEvidenceV1(source SourceEvidenceV1, policy EvidencePolicy) (NormalizedEvidenceV1, error) {
	if err := policy.validate(); err != nil {
		return NormalizedEvidenceV1{}, err
	}
	if err := policy.validateSource(source); err != nil {
		return NormalizedEvidenceV1{}, err
	}
	textMaps, err := validateSourceEvidenceV1(source)
	if err != nil {
		return NormalizedEvidenceV1{}, err
	}
	if err := policy.validateTextMaps(textMaps); err != nil {
		return NormalizedEvidenceV1{}, err
	}

	artifacts, artifactIDs, err := normalizeArtifacts(source.Artifacts)
	if err != nil {
		return NormalizedEvidenceV1{}, err
	}
	units := make([]NormalizedEvidenceUnitV1, len(source.Units))
	for index, unit := range source.Units {
		normalized, err := normalizeEvidenceUnit(unit, artifactIDs, textMaps[index])
		if err != nil {
			return NormalizedEvidenceV1{}, fmt.Errorf("normalize source evidence unit %d: %w", index, err)
		}
		units[index] = normalized
	}
	omissions, err := normalizeOmissions(source.Omissions, textMaps)
	if err != nil {
		return NormalizedEvidenceV1{}, fmt.Errorf("normalize source evidence omissions: %w", err)
	}

	result := NormalizedEvidenceV1{
		Artifacts:       artifacts,
		Completeness:    source.Completeness,
		ContractVersion: NormalizedEvidenceContractV1,
		Family:          canonicalEvidenceString(source.Family),
		Omissions:       omissions,
		UnitKind:        source.UnitKind,
		Units:           units,
	}
	_, checksum, err := marshalNormalizedEvidenceV1(result, false)
	if err != nil {
		return NormalizedEvidenceV1{}, err
	}
	result.Checksum = checksum
	return result, nil
}

func normalizeArtifacts(
	source []SourceEvidenceArtifactV1,
) ([]EvidenceArtifactV1, map[string]string, error) {
	artifacts := make([]EvidenceArtifactV1, len(source))
	providerToID := make(map[string]string, len(source))
	for index, artifact := range source {
		normalized := EvidenceArtifactV1{
			Pointer: canonicalEvidenceString(artifact.Pointer), Role: artifact.Role, SHA256: artifact.SHA256,
		}
		local, err := json.Marshal(normalized)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal evidence artifact identity: %w", err)
		}
		normalized.ID = evidenceID("artifact", string(artifact.Role), 0, local)
		artifacts[index] = normalized
		providerToID[artifact.ProviderID] = normalized.ID
	}
	slices.SortFunc(artifacts, func(left, right EvidenceArtifactV1) int {
		return strings.Compare(left.ID, right.ID)
	})
	return artifacts, providerToID, nil
}

func normalizeEvidenceUnit(
	source SourceEvidenceUnitV1,
	artifactIDs map[string]string,
	textMap evidenceTextMap,
) (NormalizedEvidenceUnitV1, error) {
	text := textMap.normalized
	confidence := normalizeConfidence(source.Confidence)
	regions, regionIDs, err := normalizeRegions(source, artifactIDs, textMap)
	if err != nil {
		return NormalizedEvidenceUnitV1{}, err
	}
	tables, err := normalizeTables(source, regionIDs, textMap)
	if err != nil {
		return NormalizedEvidenceUnitV1{}, err
	}
	omissions, err := normalizeOmissions(source.Omissions, []evidenceTextMap{textMap})
	if err != nil {
		return NormalizedEvidenceUnitV1{}, err
	}
	for index := range omissions {
		omissions[index].UnitOrder = source.Order
	}
	slices.SortFunc(omissions, compareEvidenceOmissions)
	headingPath := make([]string, len(source.HeadingPath))
	for index, heading := range source.HeadingPath {
		headingPath[index] = canonicalEvidenceString(heading)
	}
	unit := NormalizedEvidenceUnitV1{
		Confidence:  confidence,
		HeadingPath: headingPath,
		Locator: EvidenceLocatorV1{
			End: source.Locator.End, IndexOrigin: source.Locator.IndexOrigin, Kind: source.Locator.Kind,
			Name: canonicalEvidenceString(source.Locator.Name), Start: source.Locator.Start,
		},
		Omissions: omissions,
		Order:     source.Order,
		Regions:   regions,
		Tables:    tables,
		Text:      text,
	}
	local, err := json.Marshal(unit)
	if err != nil {
		return NormalizedEvidenceUnitV1{}, fmt.Errorf("marshal evidence unit identity: %w", err)
	}
	unit.ID = evidenceID("unit", string(source.Locator.Kind), source.Order, local)
	return unit, nil
}

func normalizeRegions(
	unit SourceEvidenceUnitV1,
	artifactIDs map[string]string,
	textMap evidenceTextMap,
) ([]NormalizedEvidenceRegionV1, map[string]string, error) {
	regions := make([]NormalizedEvidenceRegionV1, len(unit.Regions))
	providerToID := make(map[string]string, len(unit.Regions))
	for index, region := range unit.Regions {
		textRange, err := textMap.normalizeRange(region.TextRange)
		if err != nil {
			return nil, nil, err
		}
		normalized := NormalizedEvidenceRegionV1{
			ArtifactID: artifactIDs[region.ArtifactProviderID],
			Confidence: normalizeConfidence(region.Confidence),
			Geometry:   normalizeGeometry(region.Geometry),
			Kind:       region.Kind,
			Order:      region.Order,
			ParentID:   providerToID[region.ParentProviderID],
			TextRange:  textRange,
		}
		local, err := json.Marshal(normalized)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal evidence region identity: %w", err)
		}
		normalized.ID = evidenceID("region", string(region.Kind), region.Order, local)
		regions[index] = normalized
		providerToID[region.ProviderID] = normalized.ID
	}
	return regions, providerToID, nil
}

func normalizeTables(
	unit SourceEvidenceUnitV1,
	regionIDs map[string]string,
	textMap evidenceTextMap,
) ([]NormalizedEvidenceTableV1, error) {
	tables := make([]NormalizedEvidenceTableV1, len(unit.Tables))
	for index, table := range unit.Tables {
		cells := make([]NormalizedEvidenceTableCellV1, len(table.Cells))
		for cellIndex, cell := range table.Cells {
			textRange, err := textMap.normalizeRange(cell.TextRange)
			if err != nil {
				return nil, err
			}
			cells[cellIndex] = NormalizedEvidenceTableCellV1{
				Column: cell.Column, ColumnSpan: cell.ColumnSpan, Header: cell.Header, Order: cell.Order,
				RegionID: regionIDs[cell.RegionProviderID], Row: cell.Row, RowSpan: cell.RowSpan, TextRange: textRange,
			}
		}
		normalized := NormalizedEvidenceTableV1{
			Cells: cells, Columns: table.Columns, Order: table.Order,
			RegionID: regionIDs[table.RegionProviderID], Rows: table.Rows,
		}
		local, err := json.Marshal(normalized)
		if err != nil {
			return nil, fmt.Errorf("marshal evidence table identity: %w", err)
		}
		normalized.ID = evidenceID("table", "table", table.Order, local)
		tables[index] = normalized
	}
	return tables, nil
}

func normalizeConfidence(source *SourceEvidenceConfidenceV1) *EvidenceConfidenceV1 {
	if source == nil {
		return nil
	}
	return &EvidenceConfidenceV1{
		Interpretation: source.Interpretation,
		Maximum:        int64(math.Round(source.Maximum * float64(EvidenceFixedScale))),
		Minimum:        int64(math.Round(source.Minimum * float64(EvidenceFixedScale))),
		Scale:          EvidenceFixedScale,
		Value:          int64(math.Round(source.Value * float64(EvidenceFixedScale))),
	}
}

func normalizeGeometry(source *SourceEvidenceGeometryV1) *EvidenceGeometryV1 {
	if source == nil {
		return nil
	}
	return &EvidenceGeometryV1{
		Boxes:            slices.Clone(source.Boxes),
		CoordinateOrigin: source.CoordinateOrigin,
		CoordinateSpace:  source.CoordinateSpace,
		Height:           source.Height,
		Orientation:      source.Orientation,
		Polygons:         cloneEvidencePolygons(source.Polygons),
		Scale:            source.Scale,
		Unit:             source.Unit,
		Width:            source.Width,
	}
}

func cloneEvidencePolygons(source []EvidencePolygonV1) []EvidencePolygonV1 {
	if source == nil {
		return nil
	}
	result := make([]EvidencePolygonV1, len(source))
	for index, polygon := range source {
		result[index].Points = slices.Clone(polygon.Points)
	}
	return result
}

func normalizeOmissions(source []SourceEvidenceOmissionV1, textMaps []evidenceTextMap) ([]EvidenceOmissionV1, error) {
	result := make([]EvidenceOmissionV1, len(source))
	for index, omission := range source {
		normalized := EvidenceOmissionV1{
			Field: canonicalEvidenceString(omission.Field), Kind: omission.Kind,
			Reason: canonicalEvidenceString(omission.Reason), UnitOrder: omission.UnitOrder,
		}
		if omission.Range != nil {
			textIndex := omission.UnitOrder
			if len(textMaps) == 1 {
				textIndex = 0
			}
			textMap, ok := evidenceTextMapAt(textMaps, textIndex)
			if !ok {
				return nil, fmt.Errorf("omission %d references unknown unit", index)
			}
			textRange, err := textMap.normalizeRange(*omission.Range)
			if err != nil {
				return nil, fmt.Errorf("omission %d range: %w", index, err)
			}
			normalized.Range = &textRange
		}
		result[index] = normalized
	}
	slices.SortFunc(result, compareEvidenceOmissions)
	return result, nil
}

// MarshalNormalizedEvidenceV1 returns exact canonical JSON and its SHA-256.
func MarshalNormalizedEvidenceV1(evidence NormalizedEvidenceV1) ([]byte, string, error) {
	return marshalNormalizedEvidenceV1(evidence, true)
}

func marshalNormalizedEvidenceV1(evidence NormalizedEvidenceV1, checkExisting bool) ([]byte, string, error) {
	if err := validateNormalizedEvidenceV1(evidence); err != nil {
		return nil, "", err
	}
	existing := evidence.Checksum
	evidence.Checksum = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, "", fmt.Errorf("marshal normalized evidence v1: %w", err)
	}
	digest := sha256.Sum256(encoded)
	checksum := hex.EncodeToString(digest[:])
	if checkExisting && existing != "" && existing != checksum {
		return nil, "", errors.New("normalized evidence checksum does not match canonical bytes")
	}
	return encoded, checksum, nil
}

func validateNormalizedEvidenceV1(evidence NormalizedEvidenceV1) error {
	if evidence.ContractVersion != NormalizedEvidenceContractV1 {
		return fmt.Errorf("normalized evidence contract version must be %q", NormalizedEvidenceContractV1)
	}
	if !validEvidenceCompleteness(evidence.Completeness) || !validEvidenceUnitKind(evidence.UnitKind) ||
		len(evidence.Units) == 0 {
		return errors.New("normalized evidence identity is invalid")
	}
	if err := validateEvidenceIdentifier(evidence.Family, "document family"); err != nil {
		return err
	}
	if err := validateFamilyUnitKindForCompleteness(evidence.Family, evidence.UnitKind, evidence.Completeness); err != nil {
		return err
	}
	artifactIDs, err := validateNormalizedArtifacts(evidence.Artifacts)
	if err != nil {
		return err
	}
	textMaps := make([]evidenceTextMap, len(evidence.Units))
	documentRangeOffsets := partitionNormalizedOmissionRangeOffsets(evidence.Omissions, len(evidence.Units))
	for index, unit := range evidence.Units {
		if unit.Order != index || !validEvidenceID(unit.ID, "unit") || !utf8.ValidString(unit.Text) ||
			!norm.NFC.IsNormalString(unit.Text) || strings.Contains(unit.Text, "\r") {
			return fmt.Errorf("normalized evidence unit %d is not canonical", index)
		}
		textMaps[index] = newEvidenceTextMap(unit.Text, collectNormalizedRangeOffsets(unit, documentRangeOffsets[index]))
		if err := validateSourceLocator(evidence.Family, evidence.UnitKind, evidence.Completeness, SourceEvidenceLocatorV1{
			End: unit.Locator.End, IndexOrigin: unit.Locator.IndexOrigin, Kind: unit.Locator.Kind,
			Name: unit.Locator.Name, Start: unit.Locator.Start,
		}); err != nil {
			return fmt.Errorf("normalized evidence unit %d locator: %w", index, err)
		}
		if err := validateNormalizedConfidence(unit.Confidence); err != nil {
			return fmt.Errorf("normalized evidence unit %d: %w", index, err)
		}
		for headingIndex, heading := range unit.HeadingPath {
			if canonicalEvidenceString(heading) != heading || heading == "" {
				return fmt.Errorf("normalized evidence unit %d heading %d is not canonical", index, headingIndex)
			}
		}
		regionIDs, err := validateNormalizedRegions(index, unit, artifactIDs, textMaps[index])
		if err != nil {
			return err
		}
		if err := validateNormalizedTables(index, unit, regionIDs, textMaps[index]); err != nil {
			return err
		}
		if err := validateNormalizedOmissions(unit.Omissions, []evidenceTextMap{textMaps[index]}, index); err != nil {
			return fmt.Errorf("normalized evidence unit %d omissions: %w", index, err)
		}
		withoutID := unit
		withoutID.ID = ""
		local, err := json.Marshal(withoutID)
		if err != nil {
			return fmt.Errorf("marshal normalized evidence unit %d identity: %w", index, err)
		}
		if unit.ID != evidenceID("unit", string(unit.Locator.Kind), unit.Order, local) {
			return fmt.Errorf("normalized evidence unit %d has invalid unit ID", index)
		}
	}
	if err := validateNormalizedOmissions(evidence.Omissions, textMaps, -1); err != nil {
		return fmt.Errorf("normalized evidence omissions: %w", err)
	}
	if err := validateNormalizedCompleteness(evidence); err != nil {
		return err
	}
	return nil
}

func validateNormalizedArtifacts(artifacts []EvidenceArtifactV1) (map[string]struct{}, error) {
	ids := make(map[string]struct{}, len(artifacts))
	previousID := ""
	for index, artifact := range artifacts {
		if !validEvidenceArtifactRole(artifact.Role) || len(artifact.SHA256) != sha256.Size*2 ||
			!manifestjson.LowerHex(artifact.SHA256) {
			return nil, fmt.Errorf("normalized evidence artifact %d is invalid", index)
		}
		if err := validateArtifactPointer(artifact.Pointer); err != nil {
			return nil, fmt.Errorf("normalized evidence artifact %d: %w", index, err)
		}
		withoutID := artifact
		withoutID.ID = ""
		local, err := json.Marshal(withoutID)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized evidence artifact %d identity: %w", index, err)
		}
		if artifact.ID != evidenceID("artifact", string(artifact.Role), 0, local) {
			return nil, fmt.Errorf("normalized evidence artifact %d has invalid artifact ID", index)
		}
		if previousID != "" && artifact.ID <= previousID {
			return nil, errors.New("normalized evidence artifacts are not in canonical order")
		}
		previousID = artifact.ID
		ids[artifact.ID] = struct{}{}
	}
	return ids, nil
}

func validateNormalizedRegions(
	unitIndex int,
	unit NormalizedEvidenceUnitV1,
	artifactIDs map[string]struct{},
	textMap evidenceTextMap,
) (map[string]EvidenceRegionKind, error) {
	ids := make(map[string]EvidenceRegionKind, len(unit.Regions))
	textRunes := utf8.RuneCountInString(unit.Text)
	for index, region := range unit.Regions {
		if region.Order != index || !validEvidenceID(region.ID, "region") || !validEvidenceRegionKind(region.Kind) ||
			region.TextRange.Start < 0 || region.TextRange.End < region.TextRange.Start ||
			region.TextRange.End > textRunes {
			return nil, fmt.Errorf("normalized evidence unit %d region %d is not canonical", unitIndex, index)
		}
		mappedRange, err := textMap.normalizeRange(region.TextRange)
		if err != nil || mappedRange != region.TextRange {
			return nil, fmt.Errorf("normalized evidence unit %d region %d has invalid text range", unitIndex, index)
		}
		if region.ParentID != "" {
			if _, ok := ids[region.ParentID]; !ok {
				return nil, fmt.Errorf("normalized evidence unit %d region %d has unknown parent", unitIndex, index)
			}
		}
		if region.ArtifactID != "" {
			if _, ok := artifactIDs[region.ArtifactID]; !ok {
				return nil, fmt.Errorf("normalized evidence unit %d region %d has unknown artifact", unitIndex, index)
			}
		}
		if err := validateNormalizedConfidence(region.Confidence); err != nil {
			return nil, fmt.Errorf("normalized evidence unit %d region %d: %w", unitIndex, index, err)
		}
		if err := validateNormalizedGeometry(region.Geometry); err != nil {
			return nil, fmt.Errorf("normalized evidence unit %d region %d: %w", unitIndex, index, err)
		}
		withoutID := region
		withoutID.ID = ""
		local, err := json.Marshal(withoutID)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized evidence region %d identity: %w", index, err)
		}
		if region.ID != evidenceID("region", string(region.Kind), region.Order, local) {
			return nil, fmt.Errorf("normalized evidence unit %d region %d has invalid region ID", unitIndex, index)
		}
		ids[region.ID] = region.Kind
	}
	return ids, nil
}

func validateNormalizedTables(
	unitIndex int,
	unit NormalizedEvidenceUnitV1,
	regionIDs map[string]EvidenceRegionKind,
	textMap evidenceTextMap,
) error {
	textRunes := utf8.RuneCountInString(unit.Text)
	for index, table := range unit.Tables {
		if table.Order != index || !validEvidenceID(table.ID, "table") || table.Rows <= 0 || table.Columns <= 0 {
			return fmt.Errorf("normalized evidence unit %d table %d is not canonical", unitIndex, index)
		}
		if table.RegionID != "" && regionIDs[table.RegionID] != EvidenceRegionTable {
			return fmt.Errorf("normalized evidence unit %d table %d has invalid table region", unitIndex, index)
		}
		for cellIndex, cell := range table.Cells {
			if cell.Order != cellIndex || cell.Row < 0 || cell.Column < 0 || cell.RowSpan <= 0 || cell.ColumnSpan <= 0 ||
				cell.RowSpan > table.Rows || cell.ColumnSpan > table.Columns ||
				cell.Row > table.Rows-cell.RowSpan || cell.Column > table.Columns-cell.ColumnSpan ||
				cell.TextRange.Start < 0 || cell.TextRange.End < cell.TextRange.Start || cell.TextRange.End > textRunes {
				return fmt.Errorf("normalized evidence unit %d table %d cell %d is invalid", unitIndex, index, cellIndex)
			}
			mappedRange, err := textMap.normalizeRange(cell.TextRange)
			if err != nil || mappedRange != cell.TextRange {
				return fmt.Errorf("normalized evidence unit %d table %d cell %d has invalid text range", unitIndex, index, cellIndex)
			}
			if cell.RegionID != "" && regionIDs[cell.RegionID] != EvidenceRegionTableCell {
				return fmt.Errorf("normalized evidence unit %d table %d cell %d has invalid cell region", unitIndex, index, cellIndex)
			}
		}
		withoutID := table
		withoutID.ID = ""
		local, err := json.Marshal(withoutID)
		if err != nil {
			return fmt.Errorf("marshal normalized evidence table %d identity: %w", index, err)
		}
		if table.ID != evidenceID("table", "table", table.Order, local) {
			return fmt.Errorf("normalized evidence unit %d table %d has invalid table ID", unitIndex, index)
		}
	}
	return nil
}

func validateNormalizedConfidence(confidence *EvidenceConfidenceV1) error {
	if confidence == nil {
		return nil
	}
	if !validEvidenceConfidenceInterpretation(confidence.Interpretation) || confidence.Scale != EvidenceFixedScale ||
		confidence.Minimum >= confidence.Maximum || confidence.Value < confidence.Minimum ||
		confidence.Value > confidence.Maximum || abs64(confidence.Minimum) > 1_000_000*EvidenceFixedScale ||
		abs64(confidence.Maximum) > 1_000_000*EvidenceFixedScale {
		return errors.New("normalized evidence confidence is invalid")
	}
	if confidence.Interpretation == EvidenceConfidenceProbability &&
		(confidence.Minimum != 0 || confidence.Maximum != EvidenceFixedScale) {
		return errors.New("normalized evidence probability confidence must use the [0,1] scale")
	}
	return nil
}

func validateNormalizedGeometry(geometry *EvidenceGeometryV1) error {
	if geometry == nil {
		return nil
	}
	return validateSourceGeometry(&SourceEvidenceGeometryV1{
		Boxes: geometry.Boxes, CoordinateOrigin: geometry.CoordinateOrigin,
		CoordinateSpace: geometry.CoordinateSpace, Height: geometry.Height,
		Orientation: geometry.Orientation, Polygons: geometry.Polygons,
		Scale: geometry.Scale, Unit: geometry.Unit, Width: geometry.Width,
	})
}

func (policy EvidencePolicy) validate() error {
	if policy.maxArtifacts <= 0 || policy.maxCellsPerTable <= 0 || policy.maxDocumentChars <= 0 ||
		policy.maxOmissions <= 0 || policy.maxRegionsPerUnit <= 0 || policy.maxTablesPerUnit <= 0 ||
		policy.maxUnits <= 0 {
		return errors.New("document evidence limits must be positive; use NewEvidencePolicy")
	}
	return nil
}

func (policy EvidencePolicy) validateSource(source SourceEvidenceV1) error {
	if len(source.Artifacts) > policy.maxArtifacts || len(source.Units) > policy.maxUnits ||
		len(source.Omissions) > policy.maxOmissions {
		return errors.New("source evidence exceeds policy collection limits")
	}
	for index, unit := range source.Units {
		if len(unit.Regions) > policy.maxRegionsPerUnit ||
			len(unit.Tables) > policy.maxTablesPerUnit || len(unit.Omissions) > policy.maxOmissions {
			return fmt.Errorf("source evidence unit %d exceeds policy limits", index)
		}
		for tableIndex, table := range unit.Tables {
			if len(table.Cells) > policy.maxCellsPerTable {
				return fmt.Errorf("source evidence unit %d table %d exceeds policy cell limit", index, tableIndex)
			}
		}
	}
	return nil
}

func (policy EvidencePolicy) validateTextMaps(textMaps []evidenceTextMap) error {
	remaining := policy.maxDocumentChars
	for index, textMap := range textMaps {
		if textMap.normalizedRunes > remaining {
			return fmt.Errorf("source evidence unit %d exceeds policy character limit", index)
		}
		remaining -= textMap.normalizedRunes
	}
	return nil
}

func validateCompletenessOmissions(source SourceEvidenceV1) error {
	omissionCount := len(source.Omissions)
	for _, unit := range source.Units {
		omissionCount += len(unit.Omissions)
	}
	if source.Completeness == EvidenceComplete && omissionCount != 0 {
		return errors.New("complete source evidence cannot contain document omissions")
	}
	if source.Completeness != EvidenceComplete && omissionCount == 0 {
		return errors.New("incomplete source evidence must name its omissions")
	}
	return nil
}

func validateSourceOmissions(omissions []SourceEvidenceOmissionV1, textMaps []evidenceTextMap) error {
	return validateOmissions(omissions, textMaps, false)
}

func validateUnitOmissions(omissions []SourceEvidenceOmissionV1, unitOrder int, textMap evidenceTextMap) error {
	for index := range omissions {
		if omissions[index].UnitOrder != 0 && omissions[index].UnitOrder != unitOrder {
			return fmt.Errorf("omission %d references a different unit", index)
		}
	}
	return validateOmissions(omissions, []evidenceTextMap{textMap}, true)
}

func validateOmissions(omissions []SourceEvidenceOmissionV1, textMaps []evidenceTextMap, unitLocal bool) error {
	if len(omissions) > 100_000 {
		return errors.New("too many omissions")
	}
	for index, omission := range omissions {
		if !validEvidenceOmissionKind(omission.Kind) {
			return fmt.Errorf("omission %d has invalid kind", index)
		}
		if err := validateBoundedUTF8(omission.Reason, maxEvidenceReasonBytes, "omission reason"); err != nil ||
			strings.TrimSpace(omission.Reason) == "" {
			if err == nil {
				err = errors.New("omission reason is empty")
			}
			return fmt.Errorf("omission %d: %w", index, err)
		}
		if omission.Kind == EvidenceOmissionField {
			if err := validateEvidenceIdentifier(omission.Field, "omission field"); err != nil {
				return fmt.Errorf("omission %d: %w", index, err)
			}
		} else if omission.Field != "" {
			return fmt.Errorf("omission %d has a field outside a field omission", index)
		}
		if omission.Kind == EvidenceOmissionRange {
			if omission.Range == nil {
				return fmt.Errorf("omission %d requires a range", index)
			}
			textIndex := omission.UnitOrder
			if unitLocal {
				textIndex = 0
			}
			textMap, ok := evidenceTextMapAt(textMaps, textIndex)
			if !ok {
				return fmt.Errorf("omission %d references unknown unit", index)
			}
			if _, err := textMap.normalizeRange(*omission.Range); err != nil {
				return fmt.Errorf("omission %d range: %w", index, err)
			}
		} else if omission.Range != nil {
			return fmt.Errorf("omission %d has a range outside a range omission", index)
		}
	}
	return nil
}

func validateNormalizedOmissions(omissions []EvidenceOmissionV1, textMaps []evidenceTextMap, unitOrder int) error {
	if len(omissions) > 100_000 {
		return errors.New("too many omissions")
	}
	for index, omission := range omissions {
		if canonicalEvidenceString(omission.Field) != omission.Field ||
			canonicalEvidenceString(omission.Reason) != omission.Reason {
			return fmt.Errorf("omission %d is not canonical", index)
		}
		if !validEvidenceOmissionKind(omission.Kind) {
			return fmt.Errorf("omission %d has invalid kind", index)
		}
		if err := validateBoundedUTF8(omission.Reason, maxEvidenceReasonBytes, "omission reason"); err != nil ||
			strings.TrimSpace(omission.Reason) == "" {
			return fmt.Errorf("omission %d has an invalid reason", index)
		}
		if unitOrder >= 0 && omission.UnitOrder != unitOrder {
			return fmt.Errorf("omission %d has a noncanonical unit order", index)
		}
		if omission.Kind == EvidenceOmissionField {
			if err := validateEvidenceIdentifier(omission.Field, "omission field"); err != nil {
				return fmt.Errorf("omission %d: %w", index, err)
			}
		} else if omission.Field != "" {
			return fmt.Errorf("omission %d has a field outside a field omission", index)
		}
		if omission.Kind == EvidenceOmissionRange {
			if omission.Range == nil {
				return fmt.Errorf("omission %d requires a range", index)
			}
			textIndex := omission.UnitOrder
			if unitOrder >= 0 {
				textIndex = 0
			}
			textMap, ok := evidenceTextMapAt(textMaps, textIndex)
			if !ok {
				return fmt.Errorf("omission %d references unknown unit", index)
			}
			mappedRange, err := textMap.normalizeRange(*omission.Range)
			if err != nil || mappedRange != *omission.Range {
				return fmt.Errorf("omission %d has an invalid range", index)
			}
		} else if omission.Range != nil {
			return fmt.Errorf("omission %d has a range outside a range omission", index)
		}
		if index > 0 && compareEvidenceOmissions(omissions[index-1], omission) > 0 {
			return errors.New("omissions are not in canonical order")
		}
	}
	return nil
}

func validateNormalizedCompleteness(evidence NormalizedEvidenceV1) error {
	omissionCount := len(evidence.Omissions)
	for _, unit := range evidence.Units {
		omissionCount += len(unit.Omissions)
	}
	if evidence.Completeness == EvidenceComplete && omissionCount != 0 {
		return errors.New("complete normalized evidence cannot contain document omissions")
	}
	if evidence.Completeness != EvidenceComplete && omissionCount == 0 {
		return errors.New("incomplete normalized evidence must name its omissions")
	}
	return nil
}

func compareEvidenceOmissions(left, right EvidenceOmissionV1) int {
	if result := strings.Compare(left.Field, right.Field); result != 0 {
		return result
	}
	if result := strings.Compare(string(left.Kind), string(right.Kind)); result != 0 {
		return result
	}
	if left.Range == nil && right.Range != nil {
		return -1
	}
	if left.Range != nil && right.Range == nil {
		return 1
	}
	if left.Range != nil {
		if result := cmp.Compare(left.Range.End, right.Range.End); result != 0 {
			return result
		}
		if result := cmp.Compare(left.Range.Start, right.Range.Start); result != 0 {
			return result
		}
	}
	if result := strings.Compare(left.Reason, right.Reason); result != 0 {
		return result
	}
	return cmp.Compare(left.UnitOrder, right.UnitOrder)
}

type evidenceTextMap struct {
	boundaries      map[int]int
	normalized      string
	normalizedRunes int
	sourceRunes     int
}

func collectSourceRangeOffsets(unit SourceEvidenceUnitV1, documentOffsets []int) []int {
	offsets := append([]int{0}, documentOffsets...)
	for _, region := range unit.Regions {
		offsets = append(offsets, region.TextRange.Start, region.TextRange.End)
	}
	for _, table := range unit.Tables {
		for _, cell := range table.Cells {
			offsets = append(offsets, cell.TextRange.Start, cell.TextRange.End)
		}
	}
	for _, omission := range unit.Omissions {
		if omission.Range != nil {
			offsets = append(offsets, omission.Range.Start, omission.Range.End)
		}
	}
	return offsets
}

func collectNormalizedRangeOffsets(unit NormalizedEvidenceUnitV1, documentOffsets []int) []int {
	offsets := append([]int{0}, documentOffsets...)
	for _, region := range unit.Regions {
		offsets = append(offsets, region.TextRange.Start, region.TextRange.End)
	}
	for _, table := range unit.Tables {
		for _, cell := range table.Cells {
			offsets = append(offsets, cell.TextRange.Start, cell.TextRange.End)
		}
	}
	for _, omission := range unit.Omissions {
		if omission.Range != nil {
			offsets = append(offsets, omission.Range.Start, omission.Range.End)
		}
	}
	return offsets
}

func partitionSourceOmissionRangeOffsets(omissions []SourceEvidenceOmissionV1, unitCount int) [][]int {
	result := make([][]int, unitCount)
	for _, omission := range omissions {
		if omission.Range != nil && omission.UnitOrder >= 0 && omission.UnitOrder < unitCount {
			result[omission.UnitOrder] = append(
				result[omission.UnitOrder], omission.Range.Start, omission.Range.End,
			)
		}
	}
	return result
}

func partitionNormalizedOmissionRangeOffsets(omissions []EvidenceOmissionV1, unitCount int) [][]int {
	result := make([][]int, unitCount)
	for _, omission := range omissions {
		if omission.Range != nil && omission.UnitOrder >= 0 && omission.UnitOrder < unitCount {
			result[omission.UnitOrder] = append(
				result[omission.UnitOrder], omission.Range.Start, omission.Range.End,
			)
		}
	}
	return result
}

func newEvidenceTextMap(source string, offsets []int) evidenceTextMap {
	sourceRunes := []rune(source)
	requested := make(map[int]struct{}, len(offsets)+1)
	requested[len(sourceRunes)] = struct{}{}
	for _, offset := range offsets {
		if offset >= 0 && offset <= len(sourceRunes) {
			requested[offset] = struct{}{}
		}
	}

	intermediateBySource := make(map[int]int, len(requested))
	var intermediate strings.Builder
	intermediate.Grow(len(source))
	for index := 0; index <= len(sourceRunes); {
		if _, ok := requested[index]; ok {
			intermediateBySource[index] = intermediate.Len()
		}
		if index == len(sourceRunes) {
			break
		}
		if sourceRunes[index] == '\r' {
			intermediate.WriteByte('\n')
			if index+1 < len(sourceRunes) && sourceRunes[index+1] == '\n' {
				// A range boundary between CR and LF has no canonical meaning.
				delete(intermediateBySource, index+1)
				index += 2
				continue
			}
			index++
			continue
		}
		intermediate.WriteRune(sourceRunes[index])
		index++
	}

	sourcesByIntermediate := make(map[int][]int, len(intermediateBySource))
	for sourceOffset, intermediateOffset := range intermediateBySource {
		sourcesByIntermediate[intermediateOffset] = append(sourcesByIntermediate[intermediateOffset], sourceOffset)
	}
	boundaries := make(map[int]int, len(intermediateBySource))
	assignBoundary := func(intermediateOffset, normalizedOffset int) {
		for _, sourceOffset := range sourcesByIntermediate[intermediateOffset] {
			boundaries[sourceOffset] = normalizedOffset
		}
	}
	assignBoundary(0, 0)

	var normalized strings.Builder
	normalized.Grow(intermediate.Len())
	var iterator norm.Iter
	iterator.InitString(norm.NFC, intermediate.String())
	normalizedOffset := 0
	for !iterator.Done() {
		segment := iterator.Next()
		normalized.Write(segment)
		normalizedOffset += utf8.RuneCount(segment)
		assignBoundary(iterator.Pos(), normalizedOffset)
	}
	return evidenceTextMap{
		boundaries: boundaries, normalized: normalized.String(), normalizedRunes: normalizedOffset,
		sourceRunes: len(sourceRunes),
	}
}

func (textMap evidenceTextMap) normalizeRange(sourceRange EvidenceTextRangeV1) (EvidenceTextRangeV1, error) {
	if sourceRange.Start < 0 || sourceRange.End < sourceRange.Start || sourceRange.End > textMap.sourceRunes {
		return EvidenceTextRangeV1{}, errors.New("range is outside unit text")
	}
	start, startOK := textMap.boundaries[sourceRange.Start]
	end, endOK := textMap.boundaries[sourceRange.End]
	if !startOK || !endOK {
		return EvidenceTextRangeV1{}, errors.New("range splits a normalization boundary")
	}
	return EvidenceTextRangeV1{Start: start, End: end}, nil
}

func evidenceID(kind, subtype string, order int, local []byte) string {
	localDigest := sha256.Sum256(local)
	hash := sha256.New()
	hash.Write([]byte("docbank-normalized-evidence-id/v1\x00"))
	hash.Write([]byte(NormalizedEvidenceContractV1))
	hash.Write([]byte{'\x00'})
	hash.Write([]byte(kind))
	hash.Write([]byte{'\x00'})
	_, _ = fmt.Fprintf(hash, "%09d", order)
	hash.Write([]byte{'\x00'})
	hash.Write([]byte(subtype))
	hash.Write([]byte{'\x00'})
	hash.Write(localDigest[:])
	return kind + "_" + hex.EncodeToString(hash.Sum(nil))
}

func validateArtifactPointer(pointer string) error {
	if err := validateBoundedUTF8(pointer, maxEvidencePointerBytes, "artifact pointer"); err != nil {
		return err
	}
	parsed, err := url.Parse(pointer)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.RawPath != "" || parsed.ForceQuery || strings.Contains(pointer, "\\") || strings.Contains(pointer, "%") ||
		strings.HasPrefix(pointer, "/") || path.Clean(pointer) != pointer || pointer == "." || pointer == ".." ||
		strings.HasPrefix(pointer, "../") {
		return errors.New("artifact pointer must be a canonical relative path")
	}
	return nil
}

func evidenceTextMapAt(textMaps []evidenceTextMap, index int) (evidenceTextMap, bool) {
	if index < 0 || index >= len(textMaps) {
		return evidenceTextMap{}, false
	}
	return textMaps[index], true
}

func validateProviderID(value string, seen map[string]struct{}) error {
	if err := validateBoundedUTF8(value, maxEvidenceIdentifierBytes, "provider ID"); err != nil {
		return err
	}
	if _, exists := seen[value]; exists {
		return errors.New("provider ID is duplicated")
	}
	seen[value] = struct{}{}
	return nil
}

type sourceRegionRef struct {
	kind  EvidenceRegionKind
	order int
}

func validateSourceRegionID(
	value string,
	kind EvidenceRegionKind,
	seen map[string]sourceRegionRef,
	order int,
) error {
	if err := validateBoundedUTF8(value, maxEvidenceIdentifierBytes, "provider ID"); err != nil {
		return err
	}
	if _, exists := seen[value]; exists {
		return errors.New("provider ID is duplicated")
	}
	seen[value] = sourceRegionRef{kind: kind, order: order}
	return nil
}

func validateEvidenceText(value, subject string) error {
	if !utf8.ValidString(value) || len(value) > maxEvidenceTextBytes {
		return fmt.Errorf("%s must be bounded UTF-8", subject)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains NUL", subject)
	}
	return nil
}

func validateEvidenceIdentifier(value, subject string) error {
	if err := validateBoundedUTF8(value, maxEvidenceIdentifierBytes, subject); err != nil {
		return err
	}
	for index := range len(value) {
		char := value[index]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return fmt.Errorf("%s must use lowercase ASCII identifier characters", subject)
		}
	}
	return nil
}

func validateBoundedUTF8(value string, maxBytes int, subject string) error {
	if value == "" || !utf8.ValidString(value) || len(value) > maxBytes {
		return fmt.Errorf("%s must be non-empty bounded UTF-8", subject)
	}
	return nil
}

func canonicalEvidenceString(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return norm.NFC.String(value)
}

func locatorKindForUnit(kind EvidenceUnitKind) EvidenceLocatorKind {
	return EvidenceLocatorKind(kind)
}

func validEvidenceCompleteness(value EvidenceCompleteness) bool {
	return value == EvidenceComplete || value == EvidencePartial || value == EvidenceDegradedProvenance
}

func validEvidenceUnitKind(value EvidenceUnitKind) bool {
	return slices.Contains([]EvidenceUnitKind{
		EvidenceUnitGeneric, EvidenceUnitLine, EvidenceUnitMessage, EvidenceUnitPage, EvidenceUnitRecord,
		EvidenceUnitSection, EvidenceUnitSheet, EvidenceUnitSlide, EvidenceUnitSpine, EvidenceUnitTime,
	}, value)
}

func validEvidenceRegionKind(value EvidenceRegionKind) bool {
	return slices.Contains([]EvidenceRegionKind{
		EvidenceRegionCode, EvidenceRegionFigure, EvidenceRegionFooter, EvidenceRegionHeading, EvidenceRegionHeader,
		EvidenceRegionImage, EvidenceRegionList, EvidenceRegionParagraph, EvidenceRegionTable, EvidenceRegionTableCell,
	}, value)
}

func validEvidenceArtifactRole(value EvidenceArtifactRole) bool {
	return value == EvidenceArtifactImage || value == EvidenceArtifactMarkdown ||
		value == EvidenceArtifactStructured || value == EvidenceArtifactTranscript
}

func validEvidenceConfidenceInterpretation(value EvidenceConfidenceInterpretation) bool {
	return value == EvidenceConfidenceHigherIsBetter || value == EvidenceConfidenceLowerIsBetter ||
		value == EvidenceConfidenceProbability
}

func validEvidenceOmissionKind(value EvidenceOmissionKind) bool {
	return value == EvidenceOmissionField || value == EvidenceOmissionRange || value == EvidenceOmissionUnit
}

func validCoordinateOrigin(value EvidenceCoordinateOrigin) bool {
	return value == EvidenceCoordinateBottomLeft || value == EvidenceCoordinateTopLeft
}

func validCoordinateSpace(value EvidenceCoordinateSpace) bool {
	return value == EvidenceCoordinateImage || value == EvidenceCoordinatePage || value == EvidenceCoordinateUnit
}

func validGeometryUnit(value EvidenceGeometryUnit) bool {
	return value == EvidenceGeometryNormalized || value == EvidenceGeometryPixel || value == EvidenceGeometryPoint
}

func validEvidenceID(value, kind string) bool {
	prefix := kind + "_"
	return strings.HasPrefix(value, prefix) && len(value) == len(prefix)+sha256.Size*2 &&
		manifestjson.LowerHex(strings.TrimPrefix(value, prefix))
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func abs64(value int64) int64 {
	if value == math.MinInt64 {
		return math.MaxInt64
	}
	if value < 0 {
		return -value
	}
	return value
}
