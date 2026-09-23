package redaction

import (
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	MaxDecisionReasonBytes = 4 << 10
	MaxDecisionLabelBytes  = 256
	maxTextMapUnitIDBytes  = 256
)

func ValidateDecision(d Decision) error {
	if d.Uncertain && d.Action != "keep" {
		return &Problem{Code: "decision_conflict", DecisionIDs: []string{d.ID}}
	}
	if !canonicalUUIDv4(d.ID) || !canonicalUUIDv4(d.MemberID) {
		return decisionProblem(d.ID, "decision identity is not a canonical UUIDv4")
	}
	if d.Action != "keep" && d.Action != "redact" {
		return decisionProblem(d.ID, "unknown decision action")
	}
	if !boundedUTF8(d.Reason, MaxDecisionReasonBytes) || !boundedUTF8(d.Label, MaxDecisionLabelBytes) {
		return decisionProblem(d.ID, "decision text exceeds its UTF-8 byte limit")
	}
	if (d.Actor == "") != (d.CreatedAt == "" && d.Revision == 0) {
		return decisionProblem(d.ID, "decision provenance is incomplete")
	}
	if d.Actor != "" {
		createdAt, err := time.Parse(time.RFC3339Nano, d.CreatedAt)
		if invalidProductionActor(d.Actor) || err != nil || createdAt.Location() != time.UTC || d.Revision < 1 {
			return decisionProblem(d.ID, "decision provenance is invalid")
		}
	}
	if !canonical.IsSHA256Hex(d.Selector.MapSHA256) {
		return decisionProblem(d.ID, "selector map identity is not a SHA-256")
	}
	if err := validateSelector(d.Selector); err != nil {
		return decisionProblem(d.ID, err.Error())
	}
	return nil
}

func ValidateMap(value TextMap) error {
	if err := preflightTextMap(value); err != nil {
		return err
	}
	if value.Contract != "aligned-text/v1" {
		return mapProblem("unknown aligned-text contract")
	}
	if !canonical.IsSHA256Hex(value.SHA256) || !canonical.IsSHA256Hex(value.PDFSHA256) ||
		!canonical.IsSHA256Hex(value.EvidenceSHA256) {
		return mapProblem("map identities must be SHA-256 digests")
	}
	if !utf8.ValidString(value.Text) {
		return mapProblem("map text is not valid UTF-8")
	}
	if len(value.Pages) == 0 {
		return mapProblem("map has no pages")
	}
	if !reflect.DeepEqual(value, NormalizeTextMap(value)) {
		return mapProblem("map is not in canonical order")
	}
	pages := make(map[int]Page, len(value.Pages))
	var previousPageEnd int64
	for index, page := range value.Pages {
		if page.Number != index+1 || !canonical.IsSHA256Hex(page.FrameSHA256) || page.Width <= 0 || page.Height <= 0 {
			return mapProblem("invalid page frame")
		}
		if err := validateTextSpan(page.Span, value.Text, true); err != nil || index == 0 && page.Span.Start != 0 || index > 0 && page.Span.Start != previousPageEnd {
			return mapProblem("invalid page span")
		}
		previousPageEnd = page.Span.End
		pages[page.Number] = page
	}
	if previousPageEnd != int64(len(value.Text)) {
		return mapProblem("page spans do not cover the exact mapped text")
	}

	var previousAtomEnd int64
	for index, atom := range value.Atoms {
		if err := validateTextSpan(atom.Span, value.Text, false); err != nil || index > 0 && atom.Span.Start < previousAtomEnd {
			return mapProblem("invalid atom span")
		}
		if len(atom.Boxes) == 0 {
			return mapProblem("atom has no boxes")
		}
		for _, box := range atom.Boxes {
			page, ok := pages[box.Page]
			if !ok || atom.Span.Start < page.Span.Start || atom.Span.End > page.Span.End || validateFramedBox(box, page) != nil {
				return mapProblem("invalid atom box")
			}
		}
		previousAtomEnd = atom.Span.End
	}
	atomIndex := 0
	for offset, character := range value.Text {
		if unicode.IsSpace(character) {
			continue
		}
		end := int64(offset + utf8.RuneLen(character))
		for atomIndex < len(value.Atoms) && value.Atoms[atomIndex].Span.End <= int64(offset) {
			atomIndex++
		}
		if atomIndex >= len(value.Atoms) || value.Atoms[atomIndex].Span.Start > int64(offset) || value.Atoms[atomIndex].Span.End < end {
			return mapProblem("map has incomplete non-whitespace text coverage")
		}
	}

	unitIDs := make(map[string]struct{}, len(value.Units))
	for _, unit := range value.Units {
		if unit.ID == "" || !utf8.ValidString(unit.ID) || !validUnitKind(unit.Kind) || len(unit.Spans) == 0 || len(unit.Boxes) == 0 {
			return mapProblem("invalid semantic unit")
		}
		if _, duplicate := unitIDs[unit.ID]; duplicate {
			return mapProblem("duplicate semantic unit ID")
		}
		unitIDs[unit.ID] = struct{}{}
		var priorEnd int64
		for index, span := range unit.Spans {
			if err := validateTextSpan(span, value.Text, false); err != nil || index > 0 && span.Start < priorEnd {
				return mapProblem("invalid semantic unit span")
			}
			priorEnd = span.End
		}
		for _, box := range unit.Boxes {
			page, ok := pages[box.Page]
			if !ok || validateFramedBox(box, page) != nil {
				return mapProblem("invalid semantic unit box")
			}
		}
	}
	for _, gap := range value.Gaps {
		page, ok := pages[gap.Box.Page]
		if !ok || validateFramedBox(gap.Box, page) != nil || gap.Anchor < page.Span.Start ||
			gap.Anchor > page.Span.End || !utf8Boundary(value.Text, gap.Anchor) || gap.Unordered && gap.Anchor != page.Span.Start {
			return mapProblem("invalid gap box")
		}
	}

	_, digest, err := CanonicalTextMap(value)
	if err != nil {
		return mapProblem(fmt.Sprintf("canonicalize map: %v", err))
	}
	if digest != value.SHA256 {
		return mapProblem("map SHA-256 does not match its canonical identity")
	}
	return nil
}

// preflightTextMap bounds every collection and variable-length string before
// NormalizeTextMap can clone, sort, or derive semantic-unit boxes. It applies
// to decoded bytes and caller-built values alike.
func preflightTextMap(value TextMap) error {
	if len(value.Text) > int(maxResolveMapBytes) || len(value.Pages) > maxResolvePages ||
		len(value.Atoms) > maxResolveAtoms || len(value.Units) > maxResolveAtoms || len(value.Gaps) > maxResolveAtoms ||
		len(value.Contract) > 64 || len(value.SHA256) > 64 || len(value.PDFSHA256) > 64 || len(value.EvidenceSHA256) > 64 {
		return mapProblem("aligned-text value exceeds bounds")
	}
	total := 0
	add := func(count int) bool {
		if count < 0 || total > maxResolveInputs-count {
			return false
		}
		total += count
		return true
	}
	if !add(len(value.Pages)) || !add(len(value.Atoms)) || !add(len(value.Units)) || !add(len(value.Gaps)) {
		return mapProblem("aligned-text value exceeds bounds")
	}
	// normalizedTotal excludes caller-supplied unit boxes because normalization
	// replaces them. It includes every collection retained in the normalized map
	// and is extended below by the boxes normalization would derive.
	normalizedTotal := total
	boundedFrame := func(boxes []Box) bool {
		if !add(len(boxes)) {
			return false
		}
		for _, box := range boxes {
			if len(box.FrameSHA256) > 64 {
				return false
			}
		}
		return true
	}
	for _, page := range value.Pages {
		if len(page.FrameSHA256) > 64 {
			return mapProblem("aligned-text value exceeds bounds")
		}
	}
	for _, atom := range value.Atoms {
		if !boundedFrame(atom.Boxes) {
			return mapProblem("aligned-text value exceeds bounds")
		}
		normalizedTotal += len(atom.Boxes)
	}
	for _, unit := range value.Units {
		if len(unit.ID) > maxTextMapUnitIDBytes || len(unit.Kind) > 64 || !add(len(unit.Spans)) {
			return mapProblem("aligned-text value exceeds bounds")
		}
		normalizedTotal += len(unit.Spans)
		if !boundedFrame(unit.Boxes) {
			return mapProblem("aligned-text value exceeds bounds")
		}
	}
	for _, gap := range value.Gaps {
		if len(gap.Box.FrameSHA256) > 64 {
			return mapProblem("aligned-text value exceeds bounds")
		}
	}
	if _, exceeded, err := canonical.BoundedSize(value, maxResolveMapBytes); err != nil {
		return mapProblem(fmt.Sprintf("size aligned-text value: %v", err))
	} else if exceeded {
		return mapProblem("aligned-text value exceeds bounds")
	}

	work := 0
	for _, unit := range value.Units {
		spanCount := len(unit.Spans)
		if spanCount != 0 && len(value.Atoms) > (maxResolveWork-work)/spanCount {
			return mapProblem("aligned-text normalization work exceeds bounds")
		}
		work += len(value.Atoms) * spanCount
	}
	for _, unit := range value.Units {
		for _, atom := range value.Atoms {
			for _, span := range unit.Spans {
				if atom.Span.Start < span.End && atom.Span.End > span.Start {
					if len(atom.Boxes) > maxResolveInputs-normalizedTotal {
						return mapProblem("aligned-text derived boxes exceed bounds")
					}
					normalizedTotal += len(atom.Boxes)
					break
				}
			}
		}
	}
	return nil
}

func validateSelector(selector Selector) error {
	noSpan := selector.Span == nil
	noBoxes := selector.Boxes == nil
	noPages := selector.Pages == nil
	noUnit := selector.UnitID == ""
	switch selector.Kind {
	case "text":
		if noSpan || !noBoxes || !noPages || !noUnit {
			return errors.New("text selector has fields from another selector kind")
		}
		return validateStandaloneSpan(*selector.Span)
	case "rectangle":
		if !noSpan || noBoxes || len(selector.Boxes) == 0 || !noPages || !noUnit {
			return errors.New("rectangle selector has fields from another selector kind")
		}
		for _, box := range selector.Boxes {
			if err := validateStandaloneBox(box); err != nil {
				return err
			}
		}
		return nil
	case "paragraph", "email_message", "transcript_turn":
		if !noSpan || !noBoxes || !noPages || noUnit || !utf8.ValidString(selector.UnitID) {
			return errors.New("semantic selector has fields from another selector kind")
		}
		return nil
	case "page":
		if !noSpan || !noBoxes || noPages || len(selector.Pages) == 0 || !noUnit {
			return errors.New("page selector has fields from another selector kind")
		}
		previous := 0
		for _, page := range selector.Pages {
			if page <= previous {
				return errors.New("page selector is not strictly ordered")
			}
			previous = page
		}
		return nil
	default:
		return errors.New("unknown selector kind")
	}
}

func validateTextSpan(span Span, text string, emptyAllowed bool) error {
	if err := validateStandaloneSpan(span); err != nil {
		if emptyAllowed && span.Start >= 0 && span.Start == span.End && span.End <= int64(len(text)) && utf8Boundary(text, span.Start) {
			return nil
		}
		return err
	}
	if span.End > int64(len(text)) || !utf8Boundary(text, span.Start) || !utf8Boundary(text, span.End) {
		return errors.New("span is outside text or crosses a UTF-8 codepoint")
	}
	return nil
}

func validateStandaloneSpan(span Span) error {
	if span.Start < 0 || span.End <= span.Start {
		return errors.New("span is empty or inverted")
	}
	return nil
}

func utf8Boundary(text string, offset int64) bool {
	return offset == int64(len(text)) || offset >= 0 && offset < int64(len(text)) && utf8.RuneStart(text[offset])
}

func validateFramedBox(box Box, page Page) error {
	if err := validateStandaloneBox(box); err != nil {
		return err
	}
	if box.Page != page.Number || box.FrameSHA256 != page.FrameSHA256 || box.X1 > page.Width || box.Y1 > page.Height {
		return errors.New("box is outside its declared page frame")
	}
	return nil
}

func validateStandaloneBox(box Box) error {
	if box.Page < 1 || !canonical.IsSHA256Hex(box.FrameSHA256) || box.X0 < 0 || box.Y0 < 0 || box.X1 <= box.X0 || box.Y1 <= box.Y0 {
		return errors.New("box is empty, inverted, or has an invalid frame")
	}
	return nil
}

func validUnitKind(kind string) bool {
	return kind == "paragraph" || kind == "email_message" || kind == "transcript_turn"
}

func boundedUTF8(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value)
}

func canonicalUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	raw, err := hex.DecodeString(compact)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == compact && raw[6]>>4 == 4 && raw[8]>>6 == 2
}

func decisionProblem(id, detail string) error {
	return fmt.Errorf("%s: %w", detail, &Problem{Code: "invalid_decision", DecisionIDs: []string{id}})
}

func mapProblem(detail string) error {
	return fmt.Errorf("%s: %w", detail, &Problem{Code: "invalid_map"})
}
