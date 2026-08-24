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
	maxEvidenceArtifacts        = 10_000
	maxEvidenceUnits            = 100_000
	maxEvidenceOmissions        = 100_000
	maxEvidenceRegionsPerUnit   = 1_000_000
	maxEvidenceTablesPerUnit    = 100_000
	maxEvidenceCellsPerTable    = 1_000_000
	maxEvidenceTableDimension   = 1_000_000
	maxEvidenceHeadingDepth     = 64
	maxEvidenceHeadingBytes     = 1 << 20
	maxEvidenceGeometryBoxes    = 10_000
	maxEvidenceGeometryPolygons = 10_000
	maxEvidenceGeometryPoints   = 100_000
)

// ValidateSourceEvidenceV1 validates a bounded source-evidence/v1 manifest
// without assigning durable IDs.
func ValidateSourceEvidenceV1(source SourceEvidenceV1) error {
	_, err := validateSourceEvidenceV1(source, -1)
	return err
}

func validateSourceEvidenceV1(source SourceEvidenceV1, maxDocumentChars int) ([]evidenceTextMap, error) {
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
	if len(source.Units) == 0 || len(source.Units) > maxEvidenceUnits {
		return nil, errors.New("source evidence must contain a bounded non-empty unit list")
	}
	if err := validateSourceOmissionLimit(source, maxEvidenceOmissions); err != nil {
		return nil, err
	}
	if err := validateSourceHeadingLimits(source.Units); err != nil {
		return nil, err
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
	remainingChars := maxDocumentChars
	for index, unit := range source.Units {
		if err := validateEvidenceText(unit.Text, "unit text"); err != nil {
			return nil, fmt.Errorf("source evidence unit %d: %w", index, err)
		}
		textMap, err := newEvidenceTextMap(
			unit.Text, collectSourceRangeOffsets(unit, documentRangeOffsets[index]), remainingChars,
		)
		if err != nil {
			return nil, fmt.Errorf("source evidence unit %d exceeds policy character limit: %w", index, err)
		}
		textMaps[index] = textMap
		if remainingChars >= 0 {
			remainingChars -= textMap.normalizedRunes
		}
	}
	locatorSequence := evidenceLocatorSequence{completeness: source.Completeness}
	for index := range source.Units {
		if err := validateSourceUnit(source, index, artifactIDs, textMaps[index]); err != nil {
			return nil, err
		}
		if err := locatorSequence.add(evidenceLocatorFromSource(source.Units[index].Locator)); err != nil {
			return nil, fmt.Errorf("source evidence unit %d: %w", index, err)
		}
	}
	if err := validateSourceOmissions(source.Omissions, textMaps); err != nil {
		return nil, fmt.Errorf("source evidence omissions: %w", err)
	}
	if err := locatorSequence.requireGapOmissions(normalizeUnitOmissionLocators(source.Omissions)); err != nil {
		return nil, err
	}
	return textMaps, nil
}

func validateSourceArtifacts(artifacts []SourceEvidenceArtifactV1) (map[string]struct{}, error) {
	if len(artifacts) > maxEvidenceArtifacts {
		return nil, errors.New("source evidence has too many artifacts")
	}
	providerIDs := make(map[string]struct{}, len(artifacts))
	pointerChecksums := make(map[string]string, len(artifacts))
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
		if known, exists := pointerChecksums[artifact.Pointer]; exists && known != artifact.SHA256 {
			return nil, fmt.Errorf(
				"source evidence artifact %d conflicts with the checksum of pointer %q",
				index, artifact.Pointer,
			)
		}
		pointerChecksums[artifact.Pointer] = artifact.SHA256
		canonicalIdentity := struct {
			pointer string
			role    EvidenceArtifactRole
			sha256  string
		}{artifact.Pointer, artifact.Role, artifact.SHA256}
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

type evidenceLocatorSequence struct {
	completeness EvidenceCompleteness
	gaps         []EvidenceLocatorV1
	presentNames map[string]struct{}
	unnamed      bool
	previous     EvidenceLocatorV1
	seen         bool
}

func (sequence *evidenceLocatorSequence) add(locator EvidenceLocatorV1) error {
	if locator.Kind == EvidenceLocatorGeneric {
		return nil
	}
	if locator.Kind == EvidenceLocatorMessage || locator.Kind == EvidenceLocatorSection {
		if sequence.seen && locator.Kind != sequence.previous.Kind {
			return errors.New("locator sequence changes kind")
		}
		if !sequence.seen {
			sequence.previous = locator
			sequence.presentNames = make(map[string]struct{})
			sequence.seen = true
		}
		if strings.TrimSpace(locator.Name) == "" {
			sequence.unnamed = true
			return nil
		}
		name := canonicalEvidenceString(locator.Name)
		if _, exists := sequence.presentNames[name]; exists {
			return errors.New("locator sequence repeats a named unit")
		}
		sequence.presentNames[name] = struct{}{}
		return nil
	}
	if !sequence.seen {
		sequence.previous = locator
		sequence.seen = true
		first := int64(0)
		if locator.IndexOrigin == EvidenceIndexOriginOne {
			first = 1
		}
		if locator.Start == first {
			return nil
		}
		if sequence.completeness == EvidenceComplete {
			return errors.New("complete locator sequence does not start at its index origin")
		}
		end := locator.Start - 1
		sequence.gaps = append(sequence.gaps, evidenceLocatorGap(locator, first, end))
		return nil
	}
	if locator.Kind != sequence.previous.Kind {
		return errors.New("locator sequence changes kind")
	}
	if locator.IndexOrigin != sequence.previous.IndexOrigin {
		return errors.New("locator sequence changes index origin")
	}
	if locator.Start <= sequence.previous.End {
		return errors.New("locator sequence overlaps or regresses")
	}
	next := sequence.previous.End + 1
	if locator.Start > next {
		if sequence.completeness == EvidenceComplete {
			return errors.New("complete locator sequence has a gap")
		}
		sequence.gaps = append(sequence.gaps, evidenceLocatorGap(locator, next, locator.Start-1))
	}
	sequence.previous = locator
	return nil
}

func (sequence *evidenceLocatorSequence) requireGapOmissions(omitted []EvidenceLocatorV1) error {
	if sequence.seen && (sequence.previous.Kind == EvidenceLocatorMessage ||
		sequence.previous.Kind == EvidenceLocatorSection) {
		if len(omitted) > 0 && sequence.unnamed {
			return errors.New("named unit omission is ambiguous with an unnamed present unit")
		}
		omittedNames := make(map[string]struct{}, len(omitted))
		for index, locator := range omitted {
			if locator.Kind != sequence.previous.Kind || locator.IndexOrigin != EvidenceIndexOriginNone ||
				locator.Start != 0 || locator.End != 0 || strings.TrimSpace(locator.Name) == "" {
				return fmt.Errorf("unit omission %d does not match the named locator sequence", index)
			}
			name := canonicalEvidenceString(locator.Name)
			if _, present := sequence.presentNames[name]; present {
				return fmt.Errorf("unit omission %d overlaps a present named unit", index)
			}
			if _, duplicate := omittedNames[name]; duplicate {
				return fmt.Errorf("unit omission %d repeats a named unit", index)
			}
			omittedNames[name] = struct{}{}
		}
		return nil
	}

	for index, locator := range omitted {
		if !sequence.seen || locator.Kind != sequence.previous.Kind ||
			locator.IndexOrigin != sequence.previous.IndexOrigin {
			return fmt.Errorf("unit omission %d does not match a locator gap", index)
		}
	}
	ordered := slices.Clone(omitted)
	slices.SortFunc(ordered, func(left, right EvidenceLocatorV1) int {
		if result := cmp.Compare(left.Start, right.Start); result != 0 {
			return result
		}
		return cmp.Compare(left.End, right.End)
	})
	omissionIndex := 0
	for _, gap := range sequence.gaps {
		cursor := gap.Start
		for omissionIndex < len(ordered) && locatorStartsWithinGap(ordered[omissionIndex], gap) {
			locator := ordered[omissionIndex]
			if locator.Start != cursor || locator.End > gap.End {
				return fmt.Errorf("unit omission %d does not match a locator gap", omissionIndex)
			}
			cursor = locatorPositionAfter(locator)
			omissionIndex++
		}
		if cursor != locatorPositionAfter(gap) {
			return fmt.Errorf("partial evidence locator gap %d:%d lacks a matching unit omission", gap.Start, gap.End)
		}
	}
	trailingCursor := locatorPositionAfter(sequence.previous)
	for omissionIndex < len(ordered) {
		locator := ordered[omissionIndex]
		if locator.Start != trailingCursor {
			return fmt.Errorf("unit omission %d does not match a locator gap", omissionIndex)
		}
		trailingCursor = locatorPositionAfter(locator)
		omissionIndex++
	}
	return nil
}

func locatorStartsWithinGap(locator, gap EvidenceLocatorV1) bool {
	return locator.Start <= gap.End
}

func locatorPositionAfter(locator EvidenceLocatorV1) int64 {
	return locator.End + 1
}

func evidenceLocatorGap(locator EvidenceLocatorV1, start, end int64) EvidenceLocatorV1 {
	return EvidenceLocatorV1{
		End: end, IndexOrigin: locator.IndexOrigin, Kind: locator.Kind, Start: start,
	}
}

func normalizeUnitOmissionLocators(omissions []SourceEvidenceOmissionV1) []EvidenceLocatorV1 {
	result := make([]EvidenceLocatorV1, 0, len(omissions))
	for _, omission := range omissions {
		if omission.Kind == EvidenceOmissionUnit && omission.Locator != nil {
			result = append(result, evidenceLocatorFromSource(*omission.Locator))
		}
	}
	return result
}

func evidenceLocatorFromSource(locator SourceEvidenceLocatorV1) EvidenceLocatorV1 {
	return EvidenceLocatorV1(locator)
}

func validateSourceHeadingLimits(units []SourceEvidenceUnitV1) error {
	remaining := maxEvidenceHeadingBytes
	for index, unit := range units {
		if err := validateHeadingPathLimits(unit.HeadingPath, &remaining); err != nil {
			return fmt.Errorf("source evidence unit %d: %w", index, err)
		}
	}
	return nil
}

func validateHeadingPathLimits(headingPath []string, remainingBytes *int) error {
	if len(headingPath) > maxEvidenceHeadingDepth {
		return errors.New("evidence heading depth exceeds its limit")
	}
	for _, heading := range headingPath {
		if len(heading) > *remainingBytes {
			return errors.New("evidence heading bytes exceed their document limit")
		}
		*remainingBytes -= len(heading)
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
		if !validEvidenceFamily(family) {
			return fmt.Errorf("unknown document family %q", family)
		}
		return nil
	}
	if unitKind == EvidenceUnitGeneric {
		return errors.New("generic units must be marked degraded-provenance")
	}
	return validateFamilyUnitKind(family, unitKind)
}

func validateFamilyUnitKind(family string, unitKind EvidenceUnitKind) error {
	if !validEvidenceFamily(family) {
		return fmt.Errorf("unknown document family %q", family)
	}
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
	}
	if !allowed {
		return fmt.Errorf("document family %q cannot use unit kind %q", family, unitKind)
	}
	return nil
}

func validEvidenceFamily(family string) bool {
	switch family {
	case "pdf", "word", "presentation", "spreadsheet", "ebook", "structured", "source", "text", "mail":
		return true
	default:
		return false
	}
}

func validateSourceRegions(
	unitIndex int,
	textMap evidenceTextMap,
	regions []SourceEvidenceRegionV1,
	artifactIDs map[string]struct{},
) (map[string]sourceRegionRef, error) {
	if len(regions) > maxEvidenceRegionsPerUnit {
		return nil, fmt.Errorf("source evidence unit %d has too many regions", unitIndex)
	}
	providerIDs := make(map[string]sourceRegionRef, len(regions))
	for index, region := range regions {
		if region.Order != index {
			return nil, fmt.Errorf("source evidence unit %d has noncontiguous region order", unitIndex)
		}
		if err := validateSourceRegionID(region.ProviderID, region.Kind, region.ParentProviderID, providerIDs, index); err != nil {
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
		textRange, err := textMap.normalizeRange(region.TextRange)
		if err != nil {
			return nil, fmt.Errorf("source evidence unit %d region %d text range: %w", unitIndex, index, err)
		}
		ref := providerIDs[region.ProviderID]
		ref.textRange = textRange
		providerIDs[region.ProviderID] = ref
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
	if len(tables) > maxEvidenceTablesPerUnit {
		return fmt.Errorf("source evidence unit %d has too many tables", unitIndex)
	}
	providerIDs := make(map[string]struct{}, len(tables))
	tableRegions := make(map[string]struct{})
	cellRegions := make(map[string]struct{})
	for index, table := range tables {
		if table.Order != index {
			return fmt.Errorf("source evidence unit %d has noncontiguous table order", unitIndex)
		}
		if err := validateProviderID(table.ProviderID, providerIDs); err != nil {
			return fmt.Errorf("source evidence unit %d table %d: %w", unitIndex, index, err)
		}
		if table.Rows <= 0 || table.Columns <= 0 || table.Rows > maxEvidenceTableDimension ||
			table.Columns > maxEvidenceTableDimension {
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
			if _, used := tableRegions[table.RegionProviderID]; used {
				return fmt.Errorf("source evidence unit %d table %d region is already used by another table", unitIndex, index)
			}
			tableRegions[table.RegionProviderID] = struct{}{}
		}
		if len(table.Cells) > maxEvidenceCellsPerTable {
			return fmt.Errorf("source evidence unit %d table %d has too many cells", unitIndex, index)
		}
		for cellIndex, cell := range table.Cells {
			if err := validateSourceTableCell(textMap, table, cell, cellIndex, regionIDs, cellRegions); err != nil {
				return fmt.Errorf("source evidence unit %d table %d cell %d: %w", unitIndex, index, cellIndex, err)
			}
		}
		if evidenceTableCellsOverlap(table.Cells, func(cell SourceEvidenceTableCellV1) (int, int, int, int) {
			return cell.Row, cell.Row + cell.RowSpan, cell.Column, cell.Column + cell.ColumnSpan
		}) {
			return fmt.Errorf("source evidence unit %d table %d has overlapping cells", unitIndex, index)
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
	cellRegions map[string]struct{},
) error {
	if cell.Order != index {
		return errors.New("noncontiguous cell order")
	}
	if cell.Row < 0 || cell.Column < 0 || cell.RowSpan <= 0 || cell.ColumnSpan <= 0 ||
		cell.RowSpan > table.Rows || cell.ColumnSpan > table.Columns ||
		cell.Row > table.Rows-cell.RowSpan || cell.Column > table.Columns-cell.ColumnSpan {
		return errors.New("cell coordinates exceed table dimensions")
	}
	textRange, err := textMap.normalizeRange(cell.TextRange)
	if err != nil {
		return fmt.Errorf("text range: %w", err)
	}
	if cell.RegionProviderID != "" {
		region, ok := regionIDs[cell.RegionProviderID]
		if !ok {
			return errors.New("unknown region")
		}
		if region.kind != EvidenceRegionTableCell {
			return errors.New("invalid cell region")
		}
		if region.parent != table.RegionProviderID {
			return errors.New("cell region belongs to a different table")
		}
		if region.textRange != textRange {
			return errors.New("cell text range does not match cell region")
		}
		if _, used := cellRegions[cell.RegionProviderID]; used {
			return errors.New("cell region is already used by another cell")
		}
		cellRegions[cell.RegionProviderID] = struct{}{}
	}
	return nil
}

type evidenceTableCellEvent struct {
	columnEnd   int
	columnStart int
	delta       int32
	row         int
}

func evidenceTableCellsOverlap[T any](
	cells []T,
	bounds func(T) (rowStart, rowEnd, columnStart, columnEnd int),
) bool {
	if len(cells) < 2 {
		return false
	}
	events := make([]evidenceTableCellEvent, 0, len(cells)*2)
	columns := make([]int, 0, len(cells)*2)
	for _, cell := range cells {
		rowStart, rowEnd, columnStart, columnEnd := bounds(cell)
		columns = append(columns, columnStart, columnEnd)
		events = append(events,
			evidenceTableCellEvent{
				columnEnd: columnEnd, columnStart: columnStart, delta: 1, row: rowStart,
			},
			evidenceTableCellEvent{
				columnEnd: columnEnd, columnStart: columnStart, delta: -1, row: rowEnd,
			},
		)
	}
	slices.Sort(columns)
	columns = slices.Compact(columns)
	slices.SortFunc(events, func(left, right evidenceTableCellEvent) int {
		if result := cmp.Compare(left.row, right.row); result != 0 {
			return result
		}
		return cmp.Compare(left.delta, right.delta)
	})
	segmentCount := len(columns) - 1
	maximum := make([]int32, 4*segmentCount)
	lazy := make([]int32, 4*segmentCount)
	for _, event := range events {
		columnStart, _ := slices.BinarySearch(columns, event.columnStart)
		columnEnd, _ := slices.BinarySearch(columns, event.columnEnd)
		addEvidenceCellCoverage(maximum, lazy, 1, 0, segmentCount, columnStart, columnEnd, event.delta)
		if maximum[1] > 1 {
			return true
		}
	}
	return false
}

func addEvidenceCellCoverage(
	maximum, lazy []int32,
	node, left, right, updateLeft, updateRight int,
	delta int32,
) {
	if updateLeft <= left && right <= updateRight {
		maximum[node] += delta
		lazy[node] += delta
		return
	}
	middle := left + (right-left)/2
	if updateLeft < middle {
		addEvidenceCellCoverage(maximum, lazy, node*2, left, middle, updateLeft, updateRight, delta)
	}
	if updateRight > middle {
		addEvidenceCellCoverage(maximum, lazy, node*2+1, middle, right, updateLeft, updateRight, delta)
	}
	maximum[node] = lazy[node] + max(maximum[node*2], maximum[node*2+1])
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
	textMaps, err := validateSourceEvidenceV1(source, policy.maxDocumentChars)
	if err != nil {
		return NormalizedEvidenceV1{}, err
	}

	artifacts, artifactIDs, err := normalizeArtifacts(source.Artifacts)
	if err != nil {
		return NormalizedEvidenceV1{}, err
	}
	documentOmissions, unitRangeOmissions := partitionSourceOmissionsForNormalization(
		source.Omissions, len(source.Units),
	)
	units := make([]NormalizedEvidenceUnitV1, len(source.Units))
	for index, unit := range source.Units {
		normalized, err := normalizeEvidenceUnit(
			unit, unitRangeOmissions[index], artifactIDs, textMaps[index],
		)
		if err != nil {
			return NormalizedEvidenceV1{}, fmt.Errorf("normalize source evidence unit %d: %w", index, err)
		}
		units[index] = normalized
	}
	omissions, err := normalizeOmissions(documentOmissions, textMaps)
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
			Pointer: artifact.Pointer, Role: artifact.Role, SHA256: artifact.SHA256,
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
	documentRanges []SourceEvidenceOmissionV1,
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
	normalizedDocumentRanges, err := normalizeOmissions(documentRanges, []evidenceTextMap{textMap})
	if err != nil {
		return NormalizedEvidenceUnitV1{}, err
	}
	omissions = append(omissions, normalizedDocumentRanges...)
	for index := range omissions {
		omissions[index].UnitOrder = source.Order
	}
	slices.SortFunc(omissions, compareEvidenceOmissions)
	omissions = slices.CompactFunc(omissions, func(left, right EvidenceOmissionV1) bool {
		return compareEvidenceOmissions(left, right) == 0
	})
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
		normalized.ID = evidenceID(
			"region", fmt.Sprintf("%s/unit-%d", region.Kind, unit.Order), region.Order, local,
		)
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
		normalized.ID = evidenceID("table", fmt.Sprintf("table/unit-%d", unit.Order), table.Order, local)
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
		if omission.Locator != nil {
			locator := evidenceLocatorFromSource(*omission.Locator)
			locator.Name = canonicalEvidenceString(locator.Name)
			normalized.Locator = &locator
		}
		result[index] = normalized
	}
	slices.SortFunc(result, compareEvidenceOmissions)
	if err := validateCanonicalOmissionOrder(result); err != nil {
		return nil, err
	}
	return result, nil
}

func partitionSourceOmissionsForNormalization(
	source []SourceEvidenceOmissionV1,
	unitCount int,
) ([]SourceEvidenceOmissionV1, [][]SourceEvidenceOmissionV1) {
	documentOmissions := make([]SourceEvidenceOmissionV1, 0, len(source))
	unitRanges := make([][]SourceEvidenceOmissionV1, unitCount)
	for _, omission := range source {
		if omission.Kind == EvidenceOmissionRange && omission.Range != nil &&
			omission.UnitOrder >= 0 && omission.UnitOrder < unitCount {
			unitRanges[omission.UnitOrder] = append(unitRanges[omission.UnitOrder], omission)
			continue
		}
		documentOmissions = append(documentOmissions, omission)
	}
	return documentOmissions, unitRanges
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
		len(evidence.Units) == 0 || len(evidence.Units) > maxEvidenceUnits {
		return errors.New("normalized evidence identity is invalid")
	}
	if err := validateNormalizedOmissionLimit(evidence); err != nil {
		return err
	}
	if err := validateEvidenceIdentifier(evidence.Family, "document family"); err != nil {
		return err
	}
	if err := validateFamilyUnitKindForCompleteness(evidence.Family, evidence.UnitKind, evidence.Completeness); err != nil {
		return err
	}
	headingBytes := maxEvidenceHeadingBytes
	for index, unit := range evidence.Units {
		if err := validateHeadingPathLimits(unit.HeadingPath, &headingBytes); err != nil {
			return fmt.Errorf("normalized evidence unit %d: %w", index, err)
		}
	}
	artifactIDs, err := validateNormalizedArtifacts(evidence.Artifacts)
	if err != nil {
		return err
	}
	textMaps := make([]evidenceTextMap, len(evidence.Units))
	locatorSequence := evidenceLocatorSequence{completeness: evidence.Completeness}
	for index, unit := range evidence.Units {
		if unit.Order != index || !validEvidenceID(unit.ID, "unit") {
			return fmt.Errorf("normalized evidence unit %d is not canonical", index)
		}
		if err := validateEvidenceText(unit.Text, "unit text"); err != nil {
			return fmt.Errorf("normalized evidence unit %d: %w", index, err)
		}
		if !norm.NFC.IsNormalString(unit.Text) || strings.Contains(unit.Text, "\r") {
			return fmt.Errorf("normalized evidence unit %d text is not canonical", index)
		}
		if canonicalEvidenceString(unit.Locator.Name) != unit.Locator.Name {
			return fmt.Errorf("normalized evidence unit %d locator name is not canonical", index)
		}
		textMap, err := newEvidenceTextMap(unit.Text, collectNormalizedRangeOffsets(unit), -1)
		if err != nil {
			return fmt.Errorf("normalized evidence unit %d text map: %w", index, err)
		}
		textMaps[index] = textMap
		if err := validateSourceLocator(
			evidence.Family, evidence.UnitKind, evidence.Completeness,
			SourceEvidenceLocatorV1{
				End: unit.Locator.End, IndexOrigin: unit.Locator.IndexOrigin, Kind: unit.Locator.Kind,
				Name: unit.Locator.Name, Start: unit.Locator.Start,
			},
		); err != nil {
			return fmt.Errorf("normalized evidence unit %d locator: %w", index, err)
		}
		if err := locatorSequence.add(unit.Locator); err != nil {
			return fmt.Errorf("normalized evidence unit %d: %w", index, err)
		}
		if err := validateNormalizedConfidence(unit.Confidence); err != nil {
			return fmt.Errorf("normalized evidence unit %d: %w", index, err)
		}
		for headingIndex, heading := range unit.HeadingPath {
			if err := validateEvidenceText(heading, "heading"); err != nil ||
				canonicalEvidenceString(heading) != heading || heading == "" {
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
	if err := locatorSequence.requireGapOmissions(normalizedUnitOmissionLocators(evidence.Omissions)); err != nil {
		return err
	}
	if err := validateNormalizedCompleteness(evidence); err != nil {
		return err
	}
	return nil
}

func validateNormalizedArtifacts(artifacts []EvidenceArtifactV1) (map[string]struct{}, error) {
	if len(artifacts) > maxEvidenceArtifacts {
		return nil, errors.New("normalized evidence has too many artifacts")
	}
	ids := make(map[string]struct{}, len(artifacts))
	pointerChecksums := make(map[string]string, len(artifacts))
	previousID := ""
	for index, artifact := range artifacts {
		if !validEvidenceArtifactRole(artifact.Role) || len(artifact.SHA256) != sha256.Size*2 ||
			!manifestjson.LowerHex(artifact.SHA256) {
			return nil, fmt.Errorf("normalized evidence artifact %d is invalid", index)
		}
		if err := validateArtifactPointer(artifact.Pointer); err != nil {
			return nil, fmt.Errorf("normalized evidence artifact %d: %w", index, err)
		}
		if known, exists := pointerChecksums[artifact.Pointer]; exists && known != artifact.SHA256 {
			return nil, fmt.Errorf(
				"normalized evidence artifact %d conflicts with the checksum of pointer %q",
				index, artifact.Pointer,
			)
		}
		pointerChecksums[artifact.Pointer] = artifact.SHA256
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
) (map[string]sourceRegionRef, error) {
	if len(unit.Regions) > maxEvidenceRegionsPerUnit {
		return nil, fmt.Errorf("normalized evidence unit %d has too many regions", unitIndex)
	}
	ids := make(map[string]sourceRegionRef, len(unit.Regions))
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
		if region.ID != evidenceID(
			"region", fmt.Sprintf("%s/unit-%d", region.Kind, unitIndex), region.Order, local,
		) {
			return nil, fmt.Errorf("normalized evidence unit %d region %d has invalid region ID", unitIndex, index)
		}
		ids[region.ID] = sourceRegionRef{
			kind: region.Kind, order: index, parent: region.ParentID, textRange: region.TextRange,
		}
	}
	return ids, nil
}

func validateNormalizedTables(
	unitIndex int,
	unit NormalizedEvidenceUnitV1,
	regionIDs map[string]sourceRegionRef,
	textMap evidenceTextMap,
) error {
	if len(unit.Tables) > maxEvidenceTablesPerUnit {
		return fmt.Errorf("normalized evidence unit %d has too many tables", unitIndex)
	}
	textRunes := utf8.RuneCountInString(unit.Text)
	tableRegions := make(map[string]struct{})
	cellRegions := make(map[string]struct{})
	for index, table := range unit.Tables {
		if table.Order != index || !validEvidenceID(table.ID, "table") {
			return fmt.Errorf("normalized evidence unit %d table %d is not canonical", unitIndex, index)
		}
		if table.Rows <= 0 || table.Columns <= 0 || table.Rows > maxEvidenceTableDimension ||
			table.Columns > maxEvidenceTableDimension {
			return fmt.Errorf("normalized evidence unit %d table %d has invalid table dimensions", unitIndex, index)
		}
		if len(table.Cells) > maxEvidenceCellsPerTable {
			return fmt.Errorf("normalized evidence unit %d table %d has too many cells", unitIndex, index)
		}
		if table.RegionID != "" {
			if regionIDs[table.RegionID].kind != EvidenceRegionTable {
				return fmt.Errorf("normalized evidence unit %d table %d has invalid table region", unitIndex, index)
			}
			if _, used := tableRegions[table.RegionID]; used {
				return fmt.Errorf("normalized evidence unit %d table %d reuses a table region", unitIndex, index)
			}
			tableRegions[table.RegionID] = struct{}{}
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
			if cell.RegionID != "" {
				region, ok := regionIDs[cell.RegionID]
				if !ok || region.kind != EvidenceRegionTableCell {
					return fmt.Errorf("normalized evidence unit %d table %d cell %d has invalid cell region", unitIndex, index, cellIndex)
				}
				if region.parent != table.RegionID {
					return fmt.Errorf("normalized evidence unit %d table %d cell %d cell region belongs to a different table", unitIndex, index, cellIndex)
				}
				if region.textRange != cell.TextRange {
					return fmt.Errorf("normalized evidence unit %d table %d cell %d text range does not match cell region", unitIndex, index, cellIndex)
				}
				if _, used := cellRegions[cell.RegionID]; used {
					return fmt.Errorf("normalized evidence unit %d table %d cell %d reuses a cell region", unitIndex, index, cellIndex)
				}
				cellRegions[cell.RegionID] = struct{}{}
			}
		}
		if evidenceTableCellsOverlap(table.Cells, func(cell NormalizedEvidenceTableCellV1) (int, int, int, int) {
			return cell.Row, cell.Row + cell.RowSpan, cell.Column, cell.Column + cell.ColumnSpan
		}) {
			return fmt.Errorf("normalized evidence unit %d table %d has overlapping cells", unitIndex, index)
		}
		withoutID := table
		withoutID.ID = ""
		local, err := json.Marshal(withoutID)
		if err != nil {
			return fmt.Errorf("marshal normalized evidence table %d identity: %w", index, err)
		}
		if table.ID != evidenceID("table", fmt.Sprintf("table/unit-%d", unitIndex), table.Order, local) {
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
	if len(source.Artifacts) > policy.maxArtifacts || len(source.Units) > policy.maxUnits {
		return errors.New("source evidence exceeds policy collection limits")
	}
	if err := validateSourceOmissionLimit(source, policy.maxOmissions); err != nil {
		return err
	}
	for index, unit := range source.Units {
		if len(unit.Regions) > policy.maxRegionsPerUnit ||
			len(unit.Tables) > policy.maxTablesPerUnit {
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

func validateSourceOmissionLimit(source SourceEvidenceV1, limit int) error {
	remaining := limit
	if len(source.Omissions) > remaining {
		return errors.New("source evidence exceeds the total omission limit")
	}
	remaining -= len(source.Omissions)
	for _, unit := range source.Units {
		if len(unit.Omissions) > remaining {
			return errors.New("source evidence exceeds the total omission limit")
		}
		remaining -= len(unit.Omissions)
	}
	return nil
}

func validateNormalizedOmissionLimit(evidence NormalizedEvidenceV1) error {
	remaining := maxEvidenceOmissions
	if len(evidence.Omissions) > remaining {
		return errors.New("normalized evidence exceeds the total omission limit")
	}
	remaining -= len(evidence.Omissions)
	for _, unit := range evidence.Units {
		if len(unit.Omissions) > remaining {
			return errors.New("normalized evidence exceeds the total omission limit")
		}
		remaining -= len(unit.Omissions)
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
	if len(omissions) > maxEvidenceOmissions {
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
		if !unitLocal && omission.Kind == EvidenceOmissionField && omission.UnitOrder != 0 {
			return fmt.Errorf("omission %d field must use the global unit order", index)
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
		if omission.Kind == EvidenceOmissionUnit {
			if unitLocal {
				return fmt.Errorf("omission %d cannot omit a unit from within a unit", index)
			}
			if omission.Locator == nil {
				return fmt.Errorf("omission %d requires a locator", index)
			}
			if omission.UnitOrder != 0 {
				return fmt.Errorf("omission %d unit locator must not claim a present unit order", index)
			}
			if err := validateOmissionLocator(evidenceLocatorFromSource(*omission.Locator), false); err != nil {
				return fmt.Errorf("omission %d locator: %w", index, err)
			}
		} else if omission.Locator != nil {
			return fmt.Errorf("omission %d has a locator outside a unit omission", index)
		}
	}
	return nil
}

func validateNormalizedOmissions(omissions []EvidenceOmissionV1, textMaps []evidenceTextMap, unitOrder int) error {
	if len(omissions) > maxEvidenceOmissions {
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
		if unitOrder < 0 && omission.Kind == EvidenceOmissionField && omission.UnitOrder != 0 {
			return fmt.Errorf("omission %d field must use the global unit order", index)
		}
		if omission.Kind == EvidenceOmissionField {
			if err := validateEvidenceIdentifier(omission.Field, "omission field"); err != nil {
				return fmt.Errorf("omission %d: %w", index, err)
			}
		} else if omission.Field != "" {
			return fmt.Errorf("omission %d has a field outside a field omission", index)
		}
		if omission.Kind == EvidenceOmissionRange {
			if unitOrder < 0 {
				return fmt.Errorf("omission %d range omission must be scoped to a unit", index)
			}
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
		if omission.Kind == EvidenceOmissionUnit {
			if unitOrder >= 0 {
				return fmt.Errorf("omission %d cannot omit a unit from within a unit", index)
			}
			if omission.Locator == nil {
				return fmt.Errorf("omission %d requires a locator", index)
			}
			if omission.UnitOrder != 0 {
				return fmt.Errorf("omission %d unit locator has a noncanonical unit order", index)
			}
			if err := validateOmissionLocator(*omission.Locator, true); err != nil {
				return fmt.Errorf("omission %d locator: %w", index, err)
			}
		} else if omission.Locator != nil {
			return fmt.Errorf("omission %d has a locator outside a unit omission", index)
		}
	}
	return validateCanonicalOmissionOrder(omissions)
}

func validateOmissionLocator(locator EvidenceLocatorV1, canonical bool) error {
	if locator.Name != "" {
		if err := validateBoundedUTF8(locator.Name, maxEvidenceIdentifierBytes, "omission locator name"); err != nil {
			return err
		}
		if canonical && canonicalEvidenceString(locator.Name) != locator.Name {
			return errors.New("omission locator name is not canonical")
		}
	}
	if locator.Kind == EvidenceLocatorMessage || locator.Kind == EvidenceLocatorSection {
		if locator.IndexOrigin != EvidenceIndexOriginNone || locator.Start != 0 || locator.End != 0 ||
			strings.TrimSpace(locator.Name) == "" {
			return errors.New("omitted named unit locator is invalid")
		}
		return nil
	}
	if locator.Kind != EvidenceLocatorLine && locator.Kind != EvidenceLocatorPage &&
		locator.Kind != EvidenceLocatorRecord && locator.Kind != EvidenceLocatorSheet &&
		locator.Kind != EvidenceLocatorSlide && locator.Kind != EvidenceLocatorSpine {
		return errors.New("omitted unit locator kind is not ordered")
	}
	if locator.IndexOrigin != EvidenceIndexOriginZero && locator.IndexOrigin != EvidenceIndexOriginOne {
		return errors.New("omitted unit locator has invalid index origin")
	}
	minimum := int64(0)
	if locator.IndexOrigin == EvidenceIndexOriginOne {
		minimum = 1
	}
	if locator.Start < minimum || locator.End < locator.Start || locator.End > maxEvidenceCoordinate {
		return errors.New("omitted unit locator has an invalid range")
	}
	return nil
}

func normalizedUnitOmissionLocators(omissions []EvidenceOmissionV1) []EvidenceLocatorV1 {
	result := make([]EvidenceLocatorV1, 0, len(omissions))
	for _, omission := range omissions {
		if omission.Kind == EvidenceOmissionUnit && omission.Locator != nil {
			result = append(result, *omission.Locator)
		}
	}
	return result
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
	if left.Locator == nil && right.Locator != nil {
		return -1
	}
	if left.Locator != nil && right.Locator == nil {
		return 1
	}
	if left.Locator != nil {
		if result := compareEvidenceLocators(*left.Locator, *right.Locator); result != 0 {
			return result
		}
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

func validateCanonicalOmissionOrder(omissions []EvidenceOmissionV1) error {
	for index := 1; index < len(omissions); index++ {
		switch comparison := compareEvidenceOmissions(omissions[index-1], omissions[index]); {
		case comparison == 0:
			return errors.New("duplicate canonical omission")
		case comparison > 0:
			return errors.New("omissions are not in strict canonical order")
		}
	}
	return nil
}

func compareEvidenceLocators(left, right EvidenceLocatorV1) int {
	if result := cmp.Compare(left.End, right.End); result != 0 {
		return result
	}
	if result := strings.Compare(string(left.IndexOrigin), string(right.IndexOrigin)); result != 0 {
		return result
	}
	if result := strings.Compare(string(left.Kind), string(right.Kind)); result != 0 {
		return result
	}
	if result := strings.Compare(left.Name, right.Name); result != 0 {
		return result
	}
	return cmp.Compare(left.Start, right.Start)
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

func collectNormalizedRangeOffsets(unit NormalizedEvidenceUnitV1) []int {
	offsets := []int{0}
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

func newEvidenceTextMap(source string, offsets []int, maxRunes int) (evidenceTextMap, error) {
	requested := make(map[int]struct{}, len(offsets))
	for _, offset := range offsets {
		if offset >= 0 {
			requested[offset] = struct{}{}
		}
	}
	boundaries := make(map[int]int, len(requested)+1)
	if _, ok := requested[0]; ok {
		boundaries[0] = 0
	}

	var normalized strings.Builder
	grow := len(source)
	if maxRunes >= 0 && maxRunes < grow {
		grow = maxRunes
	}
	normalized.Grow(grow)
	var iterator norm.Iter
	iterator.InitString(norm.NFC, source)
	sourceOffset := 0
	normalizedOffset := 0
	for !iterator.Done() {
		sourceStart := iterator.Pos()
		segment := iterator.Next()
		sourceEnd := iterator.Pos()
		if len(segment) == 1 && segment[0] == '\r' {
			segment = []byte{'\n'}
			if sourceEnd < len(source) && source[sourceEnd] == '\n' {
				// A range boundary between CR and LF has no canonical meaning.
				_ = iterator.Next()
				sourceEnd = iterator.Pos()
			}
		}
		segmentRunes := utf8.RuneCount(segment)
		if maxRunes >= 0 && segmentRunes > maxRunes-normalizedOffset {
			return evidenceTextMap{}, errors.New("canonical text exceeds the remaining character budget")
		}
		normalized.Write(segment)
		normalizedOffset += segmentRunes
		sourceOffset += utf8.RuneCountInString(source[sourceStart:sourceEnd])
		if _, ok := requested[sourceOffset]; ok {
			boundaries[sourceOffset] = normalizedOffset
		}
	}
	boundaries[sourceOffset] = normalizedOffset
	return evidenceTextMap{
		boundaries: boundaries, normalized: normalized.String(), normalizedRunes: normalizedOffset,
		sourceRunes: sourceOffset,
	}, nil
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
	if canonicalEvidenceString(pointer) != pointer {
		return errors.New("artifact pointer must be canonical NFC text without carriage returns")
	}
	parsed, err := url.Parse(pointer)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.RawPath != "" || parsed.ForceQuery || strings.Contains(pointer, "\\") || strings.Contains(pointer, "%") ||
		strings.Contains(pointer, "#") ||
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
	kind      EvidenceRegionKind
	order     int
	parent    string
	textRange EvidenceTextRangeV1
}

func validateSourceRegionID(
	value string,
	kind EvidenceRegionKind,
	parent string,
	seen map[string]sourceRegionRef,
	order int,
) error {
	if err := validateBoundedUTF8(value, maxEvidenceIdentifierBytes, "provider ID"); err != nil {
		return err
	}
	if _, exists := seen[value]; exists {
		return errors.New("provider ID is duplicated")
	}
	seen[value] = sourceRegionRef{kind: kind, order: order, parent: parent}
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
		EvidenceUnitSection, EvidenceUnitSheet, EvidenceUnitSlide, EvidenceUnitSpine,
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
