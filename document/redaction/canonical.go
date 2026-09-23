package redaction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/canonical"
)

func DecodeTextMap(raw []byte) (TextMap, string, error) {
	if int64(len(raw)) > maxResolveMapBytes {
		return TextMap{}, "", mapProblem("aligned-text bytes exceed bounds")
	}
	value, err := canonical.DecodeWith(raw, func(value TextMap) ([]byte, error) {
		if err := preflightTextMap(value); err != nil {
			return nil, err
		}
		encoded, _, err := CanonicalTextMap(value)
		return encoded, err
	})
	if err != nil {
		return TextMap{}, "", err
	}
	_, checksum, err := CanonicalTextMap(value)
	if err != nil {
		return TextMap{}, "", err
	}
	value = NormalizeTextMap(value)
	value.SHA256 = checksum
	if err := ValidateMap(value); err != nil {
		return TextMap{}, "", err
	}
	return value, checksum, nil
}

func CanonicalDecisions(input []Decision) ([]byte, string, error) {
	decisions := slices.Clone(input)
	if decisions == nil {
		decisions = []Decision{}
	}
	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if err := ValidateDecision(decision); err != nil {
			return nil, "", err
		}
		if _, duplicate := seen[decision.ID]; duplicate {
			return nil, "", decisionProblem(decision.ID, "duplicate decision ID")
		}
		seen[decision.ID] = struct{}{}
	}
	slices.SortFunc(decisions, func(left, right Decision) int {
		if left.MemberID != right.MemberID {
			if left.MemberID < right.MemberID {
				return -1
			}
			return 1
		}
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	encoded, err := canonical.Marshal(decisions)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize redaction decisions: %w", err)
	}
	return encoded, sha256Hex(encoded), nil
}

// CanonicalResolved returns the canonical identity bytes and SHA-256 for a
// resolved redaction plan. The supplied top-level SHA256 field is deliberately
// excluded; every other resolved-plan field participates in the identity.
func CanonicalResolved(value Resolved) ([]byte, string, error) {
	identity := struct {
		Contract             string            `json:"contract"`
		MapSHA256            string            `json:"map_sha256"`
		RecipeSHA256         string            `json:"recipe_sha256"`
		PageCount            int               `json:"page_count"`
		Pages                []Page            `json:"pages"`
		Gaps                 []Gap             `json:"gaps"`
		RedactBoxes          []Box             `json:"redact_boxes"`
		Removed              []Span            `json:"removed"`
		Runs                 []Run             `json:"runs"`
		UncertainDecisionIDs []string          `json:"uncertain_decision_ids"`
		Regions              []RedactionRegion `json:"regions"`
	}{value.Contract, value.MapSHA256, value.RecipeSHA256, value.PageCount, value.Pages, value.Gaps, value.RedactBoxes, value.Removed,
		value.Runs, value.UncertainDecisionIDs, value.Regions}
	encoded, err := canonical.Marshal(identity)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize resolved redaction plan: %w", err)
	}
	return encoded, sha256Hex(encoded), nil
}

// CanonicalTextMap returns the self-digest-free canonical payload and digest.
// Producers use it once after canonical ordering; validators recompute it.
func CanonicalTextMap(value TextMap) ([]byte, string, error) {
	if err := preflightTextMap(value); err != nil {
		return nil, "", err
	}
	value = NormalizeTextMap(value)
	identity := struct {
		Contract       string `json:"contract"`
		PDFSHA256      string `json:"pdf_sha256"`
		EvidenceSHA256 string `json:"evidence_sha256"`
		Text           string `json:"text"`
		Pages          []Page `json:"pages"`
		Atoms          []Atom `json:"atoms"`
		Units          []Unit `json:"units"`
		Gaps           []Gap  `json:"gaps"`
	}{value.Contract, value.PDFSHA256, value.EvidenceSHA256, value.Text, value.Pages, value.Atoms, value.Units, value.Gaps}
	encoded, err := canonical.Marshal(identity)
	if err != nil {
		return nil, "", err
	}
	return encoded, sha256Hex(encoded), nil
}

// NormalizeTextMap returns the unique canonical ordering used by map producers
// and validators. Semantic unit boxes are derived from their atom spans rather
// than accepted as a second geometric authority.
func NormalizeTextMap(value TextMap) TextMap {
	value.Pages = slices.Clone(value.Pages)
	value.Atoms = slices.Clone(value.Atoms)
	value.Units = slices.Clone(value.Units)
	value.Gaps = slices.Clone(value.Gaps)
	if value.Pages == nil {
		value.Pages = []Page{}
	}
	if value.Atoms == nil {
		value.Atoms = []Atom{}
	}
	if value.Units == nil {
		value.Units = []Unit{}
	}
	if value.Gaps == nil {
		value.Gaps = []Gap{}
	}
	boxOrder := func(a, b Box) int {
		if a.Page != b.Page {
			return a.Page - b.Page
		}
		if a.Y0 < b.Y0 {
			return -1
		}
		if a.Y0 > b.Y0 {
			return 1
		}
		if a.X0 < b.X0 {
			return -1
		}
		if a.X0 > b.X0 {
			return 1
		}
		if a.Y1 < b.Y1 {
			return -1
		}
		if a.Y1 > b.Y1 {
			return 1
		}
		if a.X1 < b.X1 {
			return -1
		}
		if a.X1 > b.X1 {
			return 1
		}
		return 0
	}
	canonicalBoxes := func(boxes []Box) []Box {
		boxes = slices.Clone(boxes)
		if boxes == nil {
			boxes = []Box{}
		}
		slices.SortFunc(boxes, boxOrder)
		return slices.CompactFunc(boxes, func(a, b Box) bool { return a == b })
	}
	slices.SortFunc(value.Pages, func(a, b Page) int { return a.Number - b.Number })
	for i := range value.Atoms {
		value.Atoms[i].Boxes = canonicalBoxes(value.Atoms[i].Boxes)
	}
	slices.SortFunc(value.Atoms, func(a, b Atom) int {
		if a.Span.Start < b.Span.Start {
			return -1
		}
		if a.Span.Start > b.Span.Start {
			return 1
		}
		if a.Span.End < b.Span.End {
			return -1
		}
		if a.Span.End > b.Span.End {
			return 1
		}
		for index := range min(len(a.Boxes), len(b.Boxes)) {
			if order := boxOrder(a.Boxes[index], b.Boxes[index]); order != 0 {
				return order
			}
		}
		if len(a.Boxes) < len(b.Boxes) {
			return -1
		}
		if len(a.Boxes) > len(b.Boxes) {
			return 1
		}
		return 0
	})
	for i := range value.Units {
		value.Units[i].Spans = slices.Clone(value.Units[i].Spans)
		slices.SortFunc(value.Units[i].Spans, func(a, b Span) int {
			if a.Start < b.Start {
				return -1
			}
			if a.Start > b.Start {
				return 1
			}
			if a.End < b.End {
				return -1
			}
			if a.End > b.End {
				return 1
			}
			return 0
		})
		value.Units[i].Spans = slices.CompactFunc(value.Units[i].Spans, func(a, b Span) bool { return a == b })
		var boxes []Box
		for _, atom := range value.Atoms {
			for _, span := range value.Units[i].Spans {
				if atom.Span.Start < span.End && atom.Span.End > span.Start {
					boxes = append(boxes, atom.Boxes...)
					break
				}
			}
		}
		value.Units[i].Boxes = canonicalBoxes(boxes)
	}
	slices.SortFunc(value.Units, func(a, b Unit) int {
		as, bs := int64(0), int64(0)
		if len(a.Spans) > 0 {
			as = a.Spans[0].Start
		}
		if len(b.Spans) > 0 {
			bs = b.Spans[0].Start
		}
		if as < bs {
			return -1
		}
		if as > bs {
			return 1
		}
		if a.Kind < b.Kind {
			return -1
		}
		if a.Kind > b.Kind {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	slices.SortFunc(value.Gaps, func(a, b Gap) int {
		if a.Box.Page != b.Box.Page {
			return a.Box.Page - b.Box.Page
		}
		if a.Anchor < b.Anchor {
			return -1
		}
		if a.Anchor > b.Anchor {
			return 1
		}
		if a.Unordered != b.Unordered {
			if !a.Unordered {
				return -1
			}
			return 1
		}
		return boxOrder(a.Box, b.Box)
	})
	value.Gaps = slices.CompactFunc(value.Gaps, func(a, b Gap) bool { return a == b })
	return value
}

func canonicalTextMap(value TextMap) ([]byte, string, error) { return CanonicalTextMap(value) }

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
