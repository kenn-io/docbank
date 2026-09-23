package redaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestUncertaintyRequiresKeep(t *testing.T) {
	d := Decision{
		ID:       "11111111-1111-4111-8111-111111111111",
		MemberID: "22222222-2222-4222-8222-222222222222",
		Action:   "redact", Uncertain: true,
		Selector: Selector{Kind: "text", MapSHA256: strings.Repeat("a", 64), Span: &Span{0, 3}},
	}
	require.Error(t, ValidateDecision(d))
	d.Action = "keep"
	require.NoError(t, ValidateDecision(d))
}

func TestCanonicalDecisionsSortsByMemberThenDecisionAndRejectsDuplicates(t *testing.T) {
	decisions := []Decision{
		validDecision("33333333-3333-4333-8333-333333333333", "22222222-2222-4222-8222-222222222222"),
		validDecision("22222222-2222-4222-8222-222222222222", "11111111-1111-4111-8111-111111111111"),
		validDecision("11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111"),
	}
	original := slices.Clone(decisions)

	encoded, digest, err := CanonicalDecisions(decisions)
	require.NoError(t, err)
	require.Equal(t, original, decisions, "canonicalization must not reorder caller-owned input")
	require.Equal(t, testSHA256Hex(encoded), digest)

	var got []Decision
	require.NoError(t, json.Unmarshal(encoded, &got))
	require.Equal(t, []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}, []string{got[0].ID, got[1].ID, got[2].ID})

	reordered := []Decision{decisions[2], decisions[0], decisions[1]}
	reorderedBytes, reorderedDigest, err := CanonicalDecisions(reordered)
	require.NoError(t, err)
	require.Equal(t, encoded, reorderedBytes)
	require.Equal(t, digest, reorderedDigest)

	duplicate := append(slices.Clone(decisions), decisions[0])
	_, _, err = CanonicalDecisions(duplicate)
	require.Error(t, err)
}

func TestValidateDecisionRejectsInvalidClosedValuesAndSelectorUnionFields(t *testing.T) {
	tests := map[string]func(*Decision){
		"unknown action":   func(d *Decision) { d.Action = "remove" },
		"unknown selector": func(d *Decision) { d.Selector.Kind = "coordinates" },
		"text with rectangle": func(d *Decision) {
			d.Selector.Boxes = []Box{{Page: 1, FrameSHA256: strings.Repeat("b", 64), X1: 1, Y1: 1}}
		},
		"rectangle with span": func(d *Decision) {
			d.Selector.Kind = "rectangle"
			d.Selector.Boxes = []Box{{Page: 1, FrameSHA256: strings.Repeat("b", 64), X1: 1, Y1: 1}}
		},
		"unit with span": func(d *Decision) {
			d.Selector.Kind = "paragraph"
			d.Selector.UnitID = "paragraph-1"
		},
		"page with span": func(d *Decision) {
			d.Selector.Kind = "page"
			d.Selector.Pages = []int{1}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
			mutate(&d)
			require.Error(t, ValidateDecision(d))
		})
	}

	for _, selector := range []Selector{
		{Kind: "text", MapSHA256: strings.Repeat("a", 64), Span: &Span{Start: 0, End: 3}},
		{Kind: "rectangle", MapSHA256: strings.Repeat("a", 64), Boxes: []Box{{Page: 1, FrameSHA256: strings.Repeat("b", 64), X1: 1, Y1: 1}}},
		{Kind: "paragraph", MapSHA256: strings.Repeat("a", 64), UnitID: "paragraph-1"},
		{Kind: "email_message", MapSHA256: strings.Repeat("a", 64), UnitID: "message-1"},
		{Kind: "transcript_turn", MapSHA256: strings.Repeat("a", 64), UnitID: "turn-1"},
		{Kind: "page", MapSHA256: strings.Repeat("a", 64), Pages: []int{1, 2}},
	} {
		d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
		d.Selector = selector
		require.NoError(t, ValidateDecision(d), selector.Kind)
	}
}

func TestValidateDecisionRejectsPresentEmptyInactiveSelectorFields(t *testing.T) {
	tests := map[string]struct {
		selector Selector
		mutate   func(*Selector)
	}{
		"text": {
			selector: Selector{Kind: "text", MapSHA256: strings.Repeat("a", 64), Span: &Span{Start: 0, End: 1}},
			mutate:   func(s *Selector) { s.Boxes = []Box{} },
		},
		"rectangle": {
			selector: Selector{Kind: "rectangle", MapSHA256: strings.Repeat("a", 64), Boxes: []Box{{Page: 1, FrameSHA256: strings.Repeat("b", 64), X1: 1, Y1: 1}}},
			mutate:   func(s *Selector) { s.Pages = []int{} },
		},
		"paragraph": {
			selector: Selector{Kind: "paragraph", MapSHA256: strings.Repeat("a", 64), UnitID: "paragraph-1"},
			mutate:   func(s *Selector) { s.Boxes = []Box{} },
		},
		"email_message": {
			selector: Selector{Kind: "email_message", MapSHA256: strings.Repeat("a", 64), UnitID: "message-1"},
			mutate:   func(s *Selector) { s.Pages = []int{} },
		},
		"transcript_turn": {
			selector: Selector{Kind: "transcript_turn", MapSHA256: strings.Repeat("a", 64), UnitID: "turn-1"},
			mutate:   func(s *Selector) { s.Boxes = []Box{} },
		},
		"page": {
			selector: Selector{Kind: "page", MapSHA256: strings.Repeat("a", 64), Pages: []int{1}},
			mutate:   func(s *Selector) { s.Boxes = []Box{} },
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
			d.Selector = test.selector
			test.mutate(&d.Selector)
			require.Error(t, ValidateDecision(d))
		})
	}
}

func TestCanonicalDecisionsUsesExactSelectorKeyShape(t *testing.T) {
	tests := map[string]struct {
		selector Selector
		keys     []string
	}{
		"text": {
			Selector{Kind: "text", MapSHA256: strings.Repeat("a", 64), Span: &Span{Start: 0, End: 1}},
			[]string{"kind", "map_sha256", "span"},
		},
		"rectangle": {
			Selector{Kind: "rectangle", MapSHA256: strings.Repeat("a", 64), Boxes: []Box{{Page: 1, FrameSHA256: strings.Repeat("b", 64), X1: 1, Y1: 1}}},
			[]string{"boxes", "kind", "map_sha256"},
		},
		"paragraph": {
			Selector{Kind: "paragraph", MapSHA256: strings.Repeat("a", 64), UnitID: "paragraph-1"},
			[]string{"kind", "map_sha256", "unit_id"},
		},
		"email_message": {
			Selector{Kind: "email_message", MapSHA256: strings.Repeat("a", 64), UnitID: "message-1"},
			[]string{"kind", "map_sha256", "unit_id"},
		},
		"transcript_turn": {
			Selector{Kind: "transcript_turn", MapSHA256: strings.Repeat("a", 64), UnitID: "turn-1"},
			[]string{"kind", "map_sha256", "unit_id"},
		},
		"page": {
			Selector{Kind: "page", MapSHA256: strings.Repeat("a", 64), Pages: []int{1}},
			[]string{"kind", "map_sha256", "pages"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
			d.Selector = test.selector
			encoded, _, err := CanonicalDecisions([]Decision{d})
			require.NoError(t, err)
			var decisions []struct {
				Selector map[string]jsontext.Value `json:"selector"`
			}
			require.NoError(t, json.Unmarshal(encoded, &decisions))
			keys := make([]string, 0, len(decisions[0].Selector))
			for key := range decisions[0].Selector {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			require.Equal(t, test.keys, keys)
		})
	}
}

func TestValidateDecisionUsesUTF8ByteLimits(t *testing.T) {
	d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	d.Reason = strings.Repeat("é", 2048)
	d.Label = strings.Repeat("é", 128)
	require.NoError(t, ValidateDecision(d))

	d.Reason += "é"
	require.Error(t, ValidateDecision(d))
	d.Reason = "\xff"
	require.Error(t, ValidateDecision(d))

	d.Reason = ""
	d.Label += "é"
	require.Error(t, ValidateDecision(d))
	d.Label = "\xff"
	require.Error(t, ValidateDecision(d))
}

func TestValidateDecisionRequiresCanonicalIdentityAndSelectorCoordinates(t *testing.T) {
	tests := map[string]func(*Decision){
		"decision ID": func(d *Decision) { d.ID = "11111111-1111-1111-8111-111111111111" },
		"member ID":   func(d *Decision) { d.MemberID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA" },
		"map digest":  func(d *Decision) { d.Selector.MapSHA256 = strings.Repeat("A", 64) },
		"empty span":  func(d *Decision) { d.Selector.Span.End = d.Selector.Span.Start },
		"negative span": func(d *Decision) {
			d.Selector.Span.Start = -1
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
			mutate(&d)
			require.Error(t, ValidateDecision(d))
		})
	}

	d := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	d.Selector = Selector{Kind: "rectangle", MapSHA256: strings.Repeat("a", 64), Boxes: []Box{
		{Page: 1, FrameSHA256: strings.Repeat("b", 64), X0: math.MaxInt64, X1: math.MaxInt64 - 1, Y1: 1},
	}}
	require.Error(t, ValidateDecision(d), "inverted coordinates must not admit overflow-prone boxes")
}

func TestValidateMapChecksUTF8SpansFramesBoxesAndSelfDigest(t *testing.T) {
	require.NoError(t, ValidateMap(validTextMap()))
	invalidUTF8 := validTextMap()
	invalidUTF8.Text = "\xff"
	require.Error(t, ValidateMap(invalidUTF8))

	tests := map[string]func(*TextMap){
		"cross-codepoint atom": func(m *TextMap) {
			m.Atoms[1].Span = Span{Start: 2, End: 3}
		},
		"out-of-range atom":    func(m *TextMap) { m.Atoms[1].Span.End = 4 },
		"wrong frame identity": func(m *TextMap) { m.Atoms[0].Boxes[0].FrameSHA256 = strings.Repeat("e", 64) },
		"empty box":            func(m *TextMap) { m.Atoms[0].Boxes[0].X1 = m.Atoms[0].Boxes[0].X0 },
		"out-of-frame box":     func(m *TextMap) { m.Atoms[0].Boxes[0].X1 = m.Pages[0].Width + 1 },
		"overflow coordinate":  func(m *TextMap) { m.Atoms[0].Boxes[0].X1 = math.MaxInt64 },
		"unknown unit kind":    func(m *TextMap) { m.Units[0].Kind = "heading" },
		"duplicate unit ID": func(m *TextMap) {
			m.Units = append(m.Units, m.Units[0])
		},
		"wrong contract":   func(m *TextMap) { m.Contract = "aligned-text/v2" },
		"wrong PDF digest": func(m *TextMap) { m.PDFSHA256 = strings.Repeat("A", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := validTextMap()
			mutate(&m)
			m.SHA256 = textMapDigest(m)
			require.Error(t, ValidateMap(m))
		})
	}

	m := validTextMap()
	m.SHA256 = strings.Repeat("f", 64)
	require.Error(t, ValidateMap(m))
}

func TestDecodeTextMapRejectsOversizedRawInputBeforeDecode(t *testing.T) {
	_, _, err := DecodeTextMap(bytes.Repeat([]byte{' '}, int(maxResolveMapBytes)+1))
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "invalid_map", problem.Code)
}

func TestTextMapCanonicalizationRejectsPathologicalNormalizationBeforeWork(t *testing.T) {
	const count = 7072 // count^2 is just over the existing normalization work limit.
	value := TextMap{
		Contract:       "aligned-text/v1",
		PDFSHA256:      strings.Repeat("a", 64),
		EvidenceSHA256: strings.Repeat("b", 64),
		Text:           strings.Repeat("x", count),
		Pages:          []Page{{Number: 1, FrameSHA256: strings.Repeat("c", 64), Width: 1, Height: 1, Span: Span{End: count}}},
		Atoms:          make([]Atom, count),
		Units:          []Unit{{ID: "pathological", Kind: "paragraph", Spans: make([]Span, count)}},
	}
	for i := range count {
		// Reverse both collections so this compact payload is noncanonical and
		// cannot be accepted without first hitting the normalization guard.
		offset := int64(count - i - 1)
		value.Atoms[i].Span = Span{Start: offset, End: offset + 1}
		value.Units[0].Spans[i] = Span{Start: offset, End: offset + 1}
	}

	_, _, err := CanonicalTextMap(value)
	require.ErrorContains(t, err, "normalization work exceeds bounds")

	identity := struct {
		Contract       string `json:"contract"`
		PDFSHA256      string `json:"pdf_sha256"`
		EvidenceSHA256 string `json:"evidence_sha256"`
		Text           string `json:"text"`
		Pages          []Page `json:"pages"`
		Atoms          []Atom `json:"atoms"`
		Units          []Unit `json:"units"`
		Gaps           []Gap  `json:"gaps"`
	}{value.Contract, value.PDFSHA256, value.EvidenceSHA256, value.Text, value.Pages, value.Atoms, value.Units, []Gap{}}
	raw, err := canonical.Marshal(identity)
	require.NoError(t, err)
	require.Less(t, int64(len(raw)), maxResolveMapBytes)
	_, _, err = DecodeTextMap(raw)
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "invalid_map", problem.Code)
	require.ErrorContains(t, err, "normalization work exceeds bounds")
}

func TestCanonicalTextMapBoundsDerivedSemanticBoxes(t *testing.T) {
	const boxCount = 2000
	unitCount := maxResolveInputs/boxCount + 1
	value := TextMap{
		Contract:       "aligned-text/v1",
		PDFSHA256:      strings.Repeat("a", 64),
		EvidenceSHA256: strings.Repeat("b", 64),
		Text:           "x",
		Pages:          []Page{{Number: 1, FrameSHA256: strings.Repeat("c", 64), Width: 1, Height: 1, Span: Span{End: 1}}},
		Atoms:          []Atom{{Span: Span{End: 1}, Boxes: make([]Box, boxCount)}},
		Units:          make([]Unit, unitCount),
	}
	for i := range value.Units {
		value.Units[i] = Unit{ID: fmt.Sprintf("unit-%04d", i), Kind: "paragraph", Spans: []Span{{End: 1}}}
	}
	_, _, err := CanonicalTextMap(value)
	require.ErrorContains(t, err, "derived boxes exceed bounds")
}

func TestDecodeTextMapValidatesCanonicalPayloadAndSelfDigest(t *testing.T) {
	valid := validTextMap()
	raw, digest, err := CanonicalTextMap(valid)
	require.NoError(t, err)
	decoded, gotDigest, err := DecodeTextMap(raw)
	require.NoError(t, err)
	require.Equal(t, digest, gotDigest)
	require.Equal(t, digest, decoded.SHA256)
	require.NoError(t, ValidateMap(decoded))

	invalid := valid
	invalid.Pages = nil
	raw, _, err = CanonicalTextMap(invalid)
	require.NoError(t, err)
	_, _, err = DecodeTextMap(raw)
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "invalid_map", problem.Code)
}

func TestValidateMapBoundsVariableStringsBeforeNormalization(t *testing.T) {
	value := validTextMap()
	value.Units[0].ID = strings.Repeat("x", maxTextMapUnitIDBytes+1)
	require.ErrorContains(t, ValidateMap(value), "bounds")
}

func TestCanonicalTextMapMatchesFixedVectorAndEveryIdentityFieldParticipates(t *testing.T) {
	value := TextMap{
		Contract: "aligned-text/v1", SHA256: "ignored", PDFSHA256: "p", EvidenceSHA256: "e", Text: "x",
		Pages: []Page{}, Atoms: []Atom{}, Units: []Unit{}, Gaps: []Gap{},
	}
	encoded, digest, err := canonicalTextMap(value)
	require.NoError(t, err)
	require.Equal(t, `{"atoms":[],"contract":"aligned-text/v1","evidence_sha256":"e","gaps":[],"pages":[],"pdf_sha256":"p","text":"x","units":[]}`, string(encoded)) //nolint:testifylint // Exact canonical bytes are the durable contract.
	oracleBytes, oracleDigest := independentCanonicalSelfDigest(value)
	require.Equal(t, oracleBytes, encoded)
	require.Equal(t, oracleDigest, digest)

	value.SHA256 = "different"
	secondBytes, secondDigest, err := canonicalTextMap(value)
	require.NoError(t, err)
	require.Equal(t, encoded, secondBytes)
	require.Equal(t, digest, secondDigest)

	mutations := map[string]func(*TextMap){
		"contract":     func(v *TextMap) { v.Contract += "-changed" },
		"PDF identity": func(v *TextMap) { v.PDFSHA256 += "-changed" },
		"evidence":     func(v *TextMap) { v.EvidenceSHA256 += "-changed" },
		"text":         func(v *TextMap) { v.Text += "-changed" },
		"pages":        func(v *TextMap) { v.Pages = append(v.Pages, Page{Number: 1}) },
		"atoms":        func(v *TextMap) { v.Atoms = append(v.Atoms, Atom{}) },
		"units":        func(v *TextMap) { v.Units = append(v.Units, Unit{ID: "u"}) },
		"gaps":         func(v *TextMap) { v.Gaps = append(v.Gaps, Gap{Box: Box{Page: 1}}) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := value
			mutate(&changed)
			_, changedDigest, err := canonicalTextMap(changed)
			require.NoError(t, err)
			require.NotEqual(t, digest, changedDigest)
		})
	}
}

func TestNormalizeTextMapDeduplicatesBoxesAndRecomputesUnitBoxes(t *testing.T) {
	value := validTextMap()
	value.Atoms[0].Boxes = append(value.Atoms[0].Boxes, value.Atoms[0].Boxes[0])
	value.Units[0].Boxes = []Box{{Page: 1, FrameSHA256: value.Pages[0].FrameSHA256, X0: 1, Y0: 1, X1: 2, Y1: 2}}
	normalized := NormalizeTextMap(value)
	require.Len(t, normalized.Atoms[0].Boxes, 1)
	require.Equal(t, []Box{normalized.Atoms[0].Boxes[0], normalized.Atoms[1].Boxes[0]}, normalized.Units[0].Boxes)
	normalized.SHA256 = textMapDigest(normalized)
	require.NoError(t, ValidateMap(normalized))
}

func TestValidateMapRequiresTotalNonWhitespaceCoverage(t *testing.T) {
	value := validTextMap()
	value.Atoms = value.Atoms[:1]
	value = NormalizeTextMap(value)
	value.SHA256 = textMapDigest(value)
	require.ErrorContains(t, ValidateMap(value), "coverage")
}

func TestValidateMapBindsGapToPageAndUTF8ReadingOrderAnchor(t *testing.T) {
	value := validTextMap()
	value.Gaps = []Gap{{
		Box:    Box{Page: 1, FrameSHA256: value.Pages[0].FrameSHA256, X0: 0, Y0: 200, X1: 100, Y1: 300},
		Anchor: 1,
	}}
	value = NormalizeTextMap(value)
	value.SHA256 = textMapDigest(value)
	require.NoError(t, ValidateMap(value))

	for name, mutate := range map[string]func(*TextMap){
		"cross-codepoint": func(m *TextMap) { m.Gaps[0].Anchor = 2 },
		"outside page":    func(m *TextMap) { m.Gaps[0].Anchor = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := value
			invalid.Gaps = slices.Clone(value.Gaps)
			mutate(&invalid)
			invalid.SHA256 = textMapDigest(invalid)
			require.Error(t, ValidateMap(invalid))
		})
	}
}

func TestValidateMapRejectsEmptySemanticUnitAndCanonicalizesUnorderedGap(t *testing.T) {
	value := validTextMap()
	value.Units = []Unit{{ID: "empty", Kind: "paragraph"}}
	value = NormalizeTextMap(value)
	value.SHA256 = textMapDigest(value)
	require.ErrorContains(t, ValidateMap(value), "semantic unit")

	value = validTextMap()
	value.Gaps = []Gap{{
		Box:    Box{Page: 1, FrameSHA256: value.Pages[0].FrameSHA256, X0: 0, Y0: 200, X1: 100, Y1: 300},
		Anchor: value.Pages[0].Span.Start, Unordered: true,
	}}
	value = NormalizeTextMap(value)
	value.SHA256 = textMapDigest(value)
	require.NoError(t, ValidateMap(value))
	value.Gaps[0].Anchor++
	value.SHA256 = textMapDigest(value)
	require.Error(t, ValidateMap(value), "unordered gaps must not claim a traversal position")
}

func TestCanonicalTextMapOrdersEqualSpanAtomsByBoxes(t *testing.T) {
	value := validTextMap()
	frame := value.Pages[0].FrameSHA256
	value.Text = "A"
	value.Pages[0].Span = Span{Start: 0, End: 1}
	value.Units = nil
	value.Atoms = []Atom{
		{Span: Span{Start: 0, End: 1}, Boxes: []Box{{Page: 1, FrameSHA256: frame, X0: 100, Y0: 0, X1: 200, Y1: 100}}},
		{Span: Span{Start: 0, End: 1}, Boxes: []Box{{Page: 1, FrameSHA256: frame, X0: 0, Y0: 0, X1: 100, Y1: 100}}},
	}
	first, firstDigest, err := CanonicalTextMap(value)
	require.NoError(t, err)
	value.Atoms[0], value.Atoms[1] = value.Atoms[1], value.Atoms[0]
	second, secondDigest, err := CanonicalTextMap(value)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, firstDigest, secondDigest)
}

func TestCanonicalResolvedOmitsOnlySelfDigestAndPinsEveryIdentityField(t *testing.T) {
	value := Resolved{
		Contract: "redaction-plan/v1", MapSHA256: "m", RecipeSHA256: "r", SHA256: "ignored", PageCount: 2,
		Pages: []Page{}, Gaps: []Gap{}, RedactBoxes: []Box{}, Removed: []Span{}, Runs: []Run{},
		UncertainDecisionIDs: []string{}, Regions: []RedactionRegion{},
	}
	encoded, digest, err := CanonicalResolved(value)
	require.NoError(t, err)
	require.Equal(t, `{"contract":"redaction-plan/v1","gaps":[],"map_sha256":"m","page_count":2,"pages":[],"recipe_sha256":"r","redact_boxes":[],"regions":[],"removed":[],"runs":[],"uncertain_decision_ids":[]}`, string(encoded)) //nolint:testifylint // Exact canonical bytes are the durable contract.
	oracleBytes, oracleDigest := independentCanonicalSelfDigest(value)
	require.Equal(t, oracleBytes, encoded)
	require.Equal(t, oracleDigest, digest)

	value.SHA256 = "different"
	secondBytes, secondDigest, err := CanonicalResolved(value)
	require.NoError(t, err)
	require.Equal(t, encoded, secondBytes)
	require.Equal(t, digest, secondDigest)

	mutations := map[string]func(*Resolved){
		"contract":      func(v *Resolved) { v.Contract += "-changed" },
		"map identity":  func(v *Resolved) { v.MapSHA256 += "-changed" },
		"recipe":        func(v *Resolved) { v.RecipeSHA256 += "-changed" },
		"page count":    func(v *Resolved) { v.PageCount++ },
		"pages":         func(v *Resolved) { v.Pages = append(v.Pages, Page{Number: 1}) },
		"gaps":          func(v *Resolved) { v.Gaps = append(v.Gaps, Gap{Box: Box{Page: 1}}) },
		"redact boxes":  func(v *Resolved) { v.RedactBoxes = append(v.RedactBoxes, Box{Page: 1}) },
		"removed spans": func(v *Resolved) { v.Removed = append(v.Removed, Span{End: 1}) },
		"runs":          func(v *Resolved) { v.Runs = append(v.Runs, Run{Kind: "text"}) },
		"uncertainty":   func(v *Resolved) { v.UncertainDecisionIDs = append(v.UncertainDecisionIDs, "d") },
		"regions":       func(v *Resolved) { v.Regions = append(v.Regions, RedactionRegion{Label: "REDACTED"}) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := value
			mutate(&changed)
			_, changedDigest, err := CanonicalResolved(changed)
			require.NoError(t, err)
			require.NotEqual(t, digest, changedDigest)
		})
	}
}

func validDecision(id, memberID string) Decision {
	return Decision{
		ID: id, MemberID: memberID, Action: "keep",
		Selector: Selector{Kind: "text", MapSHA256: strings.Repeat("a", 64), Span: &Span{Start: 0, End: 3}},
	}
}

func validTextMap() TextMap {
	frame := strings.Repeat("d", 64)
	value := TextMap{
		Contract:       "aligned-text/v1",
		PDFSHA256:      strings.Repeat("b", 64),
		EvidenceSHA256: strings.Repeat("c", 64),
		Text:           "Aé",
		Pages:          []Page{{Number: 1, FrameSHA256: frame, Width: 300, Height: 72_000, Span: Span{Start: 0, End: 3}}},
		Atoms: []Atom{
			{Span: Span{Start: 0, End: 1}, Boxes: []Box{{Page: 1, FrameSHA256: frame, X0: 0, Y0: 0, X1: 100, Y1: 100}}},
			{Span: Span{Start: 1, End: 3}, Boxes: []Box{{Page: 1, FrameSHA256: frame, X0: 100, Y0: 0, X1: 300, Y1: 100}}},
		},
		Units: []Unit{{
			ID: "paragraph-1", Kind: "paragraph", Spans: []Span{{Start: 0, End: 3}},
			Boxes: []Box{
				{Page: 1, FrameSHA256: frame, X0: 0, Y0: 0, X1: 100, Y1: 100},
				{Page: 1, FrameSHA256: frame, X0: 100, Y0: 0, X1: 300, Y1: 100},
			},
		}},
		Gaps: []Gap{},
	}
	value.SHA256 = textMapDigest(value)
	return value
}

func textMapDigest(value TextMap) string {
	_, digest, err := canonicalTextMap(value)
	if err != nil {
		panic(err)
	}
	return digest
}

func independentCanonicalSelfDigest(value any) ([]byte, string) {
	full, err := canonical.Marshal(value)
	if err != nil {
		panic(err)
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(full, &members); err != nil {
		panic(err)
	}
	if _, present := members["sha256"]; !present {
		panic("canonical test value has no sha256 member")
	}
	delete(members, "sha256")
	encoded, err := canonical.Marshal(members)
	if err != nil {
		panic(err)
	}
	return encoded, testSHA256Hex(encoded)
}

func testSHA256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
