package redaction_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestKeepSelectedPreservesUncertainty(t *testing.T) {
	m := redactiontest.Map("ABC DEF GHI")
	makeKeep := func(id string, start, end int64, uncertain bool) redaction.Decision {
		return redaction.Decision{
			ID: id, MemberID: "22222222-2222-4222-8222-222222222222",
			Action: "keep", Uncertain: uncertain,
			Selector: redaction.Selector{Kind: "text", MapSHA256: m.SHA256,
				Span: &redaction.Span{Start: start, End: end}},
		}
	}
	p, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{
		makeKeep("33333333-3333-4333-8333-333333333333", 0, 3, false),
		makeKeep("44444444-4444-4444-8444-444444444444", 8, 11, true),
	}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(p)
	require.NoError(t, err)
	require.Equal(t, "ABC[REDACTED]GHI\f", string(text))
}

func TestEverySelectorResolvesInBothModes(t *testing.T) {
	m := redactiontest.Map("ABC")
	m.Units = []redaction.Unit{
		{ID: "paragraph-b", Kind: "paragraph", Spans: []redaction.Span{{Start: 1, End: 2}}},
		{ID: "message-b", Kind: "email_message", Spans: []redaction.Span{{Start: 1, End: 2}}},
		{ID: "turn-b", Kind: "transcript_turn", Spans: []redaction.Span{{Start: 1, End: 2}}},
	}
	m = sealMap(t, m)
	selectors := map[string]redaction.Selector{
		"text":            {Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}},
		"rectangle":       {Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[1].Boxes[0]}},
		"paragraph":       {Kind: "paragraph", MapSHA256: m.SHA256, UnitID: "paragraph-b"},
		"email message":   {Kind: "email_message", MapSHA256: m.SHA256, UnitID: "message-b"},
		"transcript turn": {Kind: "transcript_turn", MapSHA256: m.SHA256, UnitID: "turn-b"},
		"page":            {Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}},
	}
	for name, selector := range selectors {
		for _, mode := range []string{"redact_selected", "keep_selected"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				action := "redact"
				want := "A[REDACTED]C\f"
				if mode == "keep_selected" {
					action = "keep"
					want = "[REDACTED]B[REDACTED]\f"
				}
				if selector.Kind == "page" {
					if action == "redact" {
						want = "[REDACTED]\f"
					} else {
						want = "ABC\f"
					}
				}
				plan, err := redaction.Resolve(m, mode, []redaction.Decision{decision(1, action, selector)}, testRecipe())
				require.NoError(t, err)
				text, err := redaction.Text(plan)
				require.NoError(t, err)
				require.Equal(t, want, string(text))
			})
		}
	}
}

func TestResolverUsesOccurrenceOffsetsInsteadOfQuoteMatching(t *testing.T) {
	m := redactiontest.Map("SAME SAME")
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{
		decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 5, End: 9}}),
	}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "SAME [REDACTED]\f", string(text))
}

func TestResolverRejectsStaleMapFrameAndPartialAtom(t *testing.T) {
	m := redactiontest.Map("AB")
	stale := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: strings.Repeat("f", 64), Span: &redaction.Span{Start: 0, End: 1}})
	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{stale}, testRecipe())
	requireProblem(t, err, "source_stale")

	wrongFrame := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{
		{Page: 1, FrameSHA256: strings.Repeat("e", 64), X1: 100, Y1: 100},
	}})
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{wrongFrame}, testRecipe())
	requireProblem(t, err, "source_stale")

	m.Atoms = []redaction.Atom{{Span: redaction.Span{Start: 0, End: 2}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X1: 200, Y1: 100}}}}
	m = sealMap(t, m)
	partial := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{partial}, testRecipe())
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "selection_expansion_required", problem.Code)
	require.Equal(t, m.SHA256, problem.ExpandedMapSHA256)
	require.Equal(t, &redaction.Span{Start: 0, End: 2}, problem.Expanded.Span)
	require.Equal(t, []redaction.Span{{Start: 0, End: 2}}, problem.ExpandedSpans)
	require.Equal(t, m.Atoms[0].Boxes, problem.ExpandedBoxes)

	m = redactiontest.Map("A")
	partialRectangle := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{{
		Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 0, X1: 50, Y1: 100,
	}}})
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{partialRectangle}, testRecipe())
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "selection_expansion_required", problem.Code)
	require.Equal(t, m.SHA256, problem.ExpandedMapSHA256)
	require.Equal(t, []redaction.Box{m.Atoms[0].Boxes[0]}, problem.Expanded.Boxes)
	require.Equal(t, []redaction.Span{{Start: 0, End: 1}}, problem.ExpandedSpans)
	require.Equal(t, []redaction.Box{m.Atoms[0].Boxes[0]}, problem.ExpandedBoxes)
}

func TestSemanticExpansionReportsExactDisjointSpansAndBoxes(t *testing.T) {
	frame := strings.Repeat("d", 64)
	m := redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), Text: "AB C",
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: 400, Height: 1000, Span: redaction.Span{Start: 0, End: 4}}},
		Atoms: []redaction.Atom{
			{Span: redaction.Span{Start: 0, End: 2}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X1: 200, Y1: 100}}},
			{Span: redaction.Span{Start: 3, End: 4}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: 300, X1: 400, Y1: 100}}},
		},
		Units: []redaction.Unit{{ID: "disjoint", Kind: "paragraph", Spans: []redaction.Span{{Start: 0, End: 1}, {Start: 3, End: 4}}}},
	}
	m = sealMap(t, m)
	selectUnit := decision(1, "redact", redaction.Selector{Kind: "paragraph", MapSHA256: m.SHA256, UnitID: "disjoint"})
	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{selectUnit}, testRecipe())
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "selection_expansion_required", problem.Code)
	require.Equal(t, m.SHA256, problem.ExpandedMapSHA256)
	require.Equal(t, []redaction.Span{{Start: 0, End: 2}, {Start: 3, End: 4}}, problem.ExpandedSpans)
	require.Equal(t, []redaction.Box{m.Atoms[0].Boxes[0], m.Atoms[1].Boxes[0]}, problem.ExpandedBoxes)
}

func TestAtomClosureAndBoundaryTouching(t *testing.T) {
	m := redactiontest.Map("ABC")
	// A and B only touch; selecting A must not capture B.
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{
		decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[0].Boxes[0]}}),
	}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]BC\f", string(text))

	// A's box overlaps B's first footprint, whose second footprint overlaps C.
	frame := m.Pages[0].FrameSHA256
	m.Atoms = []redaction.Atom{
		{Span: redaction.Span{Start: 0, End: 1}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: 0, X1: 120, Y1: 100}}},
		{Span: redaction.Span{Start: 1, End: 2}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: 100, X1: 220, Y1: 100}, {Page: 1, FrameSHA256: frame, X0: 200, X1: 320, Y1: 100}}},
		{Span: redaction.Span{Start: 2, End: 3}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: 300, X1: 400, Y1: 100}}},
	}
	m.Pages[0].Width = 400
	m = sealMap(t, m)
	expand := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{expand}, testRecipe())
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "selection_expansion_required", problem.Code)
	require.Equal(t, []redaction.Span{{Start: 0, End: 3}}, problem.ExpandedSpans)
	expand.Selector = *problem.Expanded
	plan, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{expand}, testRecipe())
	require.NoError(t, err)
	text, err = redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]\f", string(text))
}

func TestExplicitConflictsAreOrderIndependent(t *testing.T) {
	m := redactiontest.Map("AB")
	keep := decision(1, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	redact := decision(2, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[0].Boxes[0]}})
	for _, decisions := range [][]redaction.Decision{{keep, redact}, {redact, keep}} {
		_, err := redaction.Resolve(m, "redact_selected", decisions, testRecipe())
		var problem *redaction.Problem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, "decision_conflict", problem.Code)
		require.Equal(t, []string{keep.ID, redact.ID}, problem.DecisionIDs)
	}

	left, right := decision(3, "redact", keep.Selector), decision(4, "redact", keep.Selector)
	left.Label, right.Label = "PRIVILEGED", "IRRELEVANT"
	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{right, left}, testRecipe())
	requireProblem(t, err, "decision_conflict")
	right.Label = left.Label
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{right, left}, testRecipe())
	require.NoError(t, err)
	require.Len(t, plan.Regions, 1, "compatible overlapping redactions coalesce")
	require.Equal(t, []string{left.ID, right.ID}, plan.Regions[0].DecisionIDs)
}

func TestAdjacentCompatibleRedactionsCoalesce(t *testing.T) {
	m := redactiontest.Map("AB")
	frame := m.Pages[0].FrameSHA256
	m.Pages[0].Width = 200
	m.Atoms[0].Boxes[0] = redaction.Box{Page: 1, FrameSHA256: frame, X1: 100, Y1: 100}
	m.Atoms[1].Boxes[0] = redaction.Box{Page: 1, FrameSHA256: frame, X0: 100, X1: 200, Y1: 100}
	m = sealMap(t, m)
	left := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	right := decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
	left.Label, right.Label = "PRIVILEGED", "PRIVILEGED"
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{right, left}, testRecipe())
	require.NoError(t, err)
	require.Len(t, plan.RedactBoxes, 1)
	require.Len(t, plan.Regions, 1)
	require.Equal(t, []string{left.ID, right.ID}, plan.Regions[0].DecisionIDs)
}

func TestKeepModeEmptyAuthorityAndWholePageOutcomes(t *testing.T) {
	m := redactiontest.Map("ABC")
	_, err := redaction.Resolve(m, "keep_selected", nil, testRecipe())
	requireProblem(t, err, "decision_conflict")

	whole := decision(1, "redact", redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}})
	plan, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{whole}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]\f", string(text))

	keep := decision(2, "keep", redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}})
	plan, err = redaction.Resolve(m, "keep_selected", []redaction.Decision{keep}, testRecipe())
	require.NoError(t, err)
	text, err = redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "ABC\f", string(text))
}

func TestCrossPageDisjointSelectionsPreservePageBoundaries(t *testing.T) {
	m := pagedMap(t, "ABCD", []int{0, 2, 4})
	plan, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{
		decision(1, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}}),
		decision(2, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 3, End: 4}}),
	}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "A[REDACTED]\f[REDACTED]D\f", string(text))
}

func TestGapMustBeMaskedAndUsesRecordedAnchorNotGeometry(t *testing.T) {
	m := redactiontest.Map("AB")
	m.Pages[0].Width = 2000
	gapBox := redaction.Box{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 0, Y0: 500, X1: 50, Y1: 550}
	m.Gaps = []redaction.Gap{{Box: gapBox, Anchor: 1}}
	m = sealMap(t, m)
	_, err := redaction.Resolve(m, "redact_selected", nil, testRecipe())
	requireProblem(t, err, "mapping_incomplete")

	mask := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{gapBox}})
	disjoint := decision(2, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{{
		Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 500, Y0: 500, X1: 550, Y1: 550,
	}}})
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{mask, disjoint}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "A[REDACTED]B\f", string(text), "the gap is geometrically first but its retained anchor is between A and B")
}

func TestFullyGraphicalRedactedPageEmitsOneMarker(t *testing.T) {
	frame := strings.Repeat("d", 64)
	m := redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: 1000, Height: 1000}},
		Gaps:  []redaction.Gap{{Box: redaction.Box{Page: 1, FrameSHA256: frame, X1: 1000, Y1: 1000}, Anchor: 0}},
	}
	m = sealMap(t, m)
	whole := decision(1, "redact", redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}})
	plan, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{whole}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]\f", string(text))
	require.Len(t, plan.Runs, 1)
}

func TestTrulyBlankRedactedPageEmitsOneMarker(t *testing.T) {
	frame := strings.Repeat("d", 64)
	m := sealMap(t, redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: 1000, Height: 1000}},
	})
	whole := decision(1, "redact", redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}})
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{whole}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]\f", string(text))
	require.Len(t, plan.Runs, 1)
}

func TestEmptySemanticUnitIsRejectedBeforeKeepAuthorization(t *testing.T) {
	m := redactiontest.Map("A")
	m.Units = []redaction.Unit{{ID: "empty", Kind: "paragraph"}}
	m = sealMapWithoutValidation(t, m)
	keep := decision(1, "keep", redaction.Selector{Kind: "paragraph", MapSHA256: m.SHA256, UnitID: "empty"})
	_, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{keep}, testRecipe())
	requireProblem(t, err, "invalid_map")
}

func TestExpansionUsesCombinedDecisionAndModeVisibility(t *testing.T) {
	m := redactiontest.Map("AB")
	partial := redaction.Box{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 0, Y0: 0, X1: 50, Y1: 100}
	partialRedact := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{partial}})
	wholeRedact := decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	partialRedact.Label, wholeRedact.Label = "SAME", "SAME"
	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{partialRedact, wholeRedact}, testRecipe())
	require.NoError(t, err, "an atom explicitly covered by another accepted redaction is not a new expansion")

	keepB := decision(3, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
	_, err = redaction.Resolve(m, "keep_selected", []redaction.Decision{partialRedact, keepB}, testRecipe())
	require.NoError(t, err, "redact expansion into keep-mode default-masked content needs no acceptance")
}

func TestExpansionUsesCollectiveAcceptedCoverage(t *testing.T) {
	t.Run("spans", func(t *testing.T) {
		m := redactiontest.Map("AB")
		m.Atoms = []redaction.Atom{{Span: redaction.Span{Start: 0, End: 2}, Boxes: []redaction.Box{{
			Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X1: 100, Y1: 100,
		}}}}
		m = sealMap(t, m)
		left := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
		right := decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
		left.Label, right.Label = "SAME", "SAME"

		_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{left, right}, testRecipe())
		require.NoError(t, err, "two accepted partial spans jointly cover the expanded atom")
	})

	t.Run("boxes", func(t *testing.T) {
		m := redactiontest.Map("A")
		m.Pages[0].Width = 1000
		m.Atoms[0].Boxes = []redaction.Box{
			{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 100, X1: 200, Y1: 100},
			{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 800, X1: 900, Y1: 100},
		}
		m = sealMap(t, m)
		left := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[0].Boxes[0]}})
		right := decision(2, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[0].Boxes[1]}})
		left.Label, right.Label = "SAME", "SAME"

		_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{left, right}, testRecipe())
		require.NoError(t, err, "two accepted partial box sets jointly cover the expanded atom")
	})
}

func TestDifferentlyLabeledEdgeTouchingRedactionsDoNotConflict(t *testing.T) {
	m := redactiontest.Map("AB")
	frame := m.Pages[0].FrameSHA256
	m.Pages[0].Width = 2000
	m.Atoms[0].Boxes[0] = redaction.Box{Page: 1, FrameSHA256: frame, X0: 1000, Y0: 100, X1: 1100, Y1: 200}
	m.Atoms[1].Boxes[0] = redaction.Box{Page: 1, FrameSHA256: frame, X0: 1234, Y0: 100, X1: 1334, Y1: 200}
	m = sealMap(t, m)
	left := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[0].Boxes[0]}})
	right := decision(2, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Atoms[1].Boxes[0]}})
	left.Label, right.Label = "PRIVILEGED", "IRRELEVANT"
	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{left, right}, testRecipe())
	require.NoError(t, err)
}

func TestMultiBoxAtomProducesOneSourceOccurrence(t *testing.T) {
	m := redactiontest.Map("A")
	m.Pages[0].Width = 300
	m.Atoms[0].Boxes = []redaction.Box{
		{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 0, X1: 100, Y1: 100},
		{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 200, X1: 300, Y1: 100},
	}
	m = sealMap(t, m)
	keep := decision(1, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	plan, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{keep}, testRecipe())
	require.NoError(t, err)
	require.Len(t, plan.Runs, 1)
	require.Equal(t, "A", plan.Runs[0].Text)
	require.Len(t, plan.Runs[0].Boxes, 2)
}

func TestQualifiedRecipePaddingAndPixelRoundTrip(t *testing.T) {
	for _, dpi := range []int{300, 600} {
		t.Run(strconv.Itoa(dpi), func(t *testing.T) {
			recipe, err := pdfproduction.QualifiedRecipeForDPI(dpi)
			require.NoError(t, err)
			m := redactiontest.Map("AB")
			m.Pages[0].Width, m.Pages[0].Height = 1000, 1000
			m.Atoms = []redaction.Atom{
				{Span: redaction.Span{Start: 0, End: 1}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 101, Y0: 101, X1: 132, Y1: 132}}},
				{Span: redaction.Span{Start: 1, End: 2}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 200, Y0: 101, X1: 231, Y1: 132}}},
			}
			m = sealMap(t, m)
			keep := decision(1, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
			plan, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{keep}, recipe)
			require.NoError(t, err)
			keptPixels := pixels(m.Atoms[1].Boxes[0], dpi)
			for _, box := range plan.RedactBoxes {
				require.False(t, overlaps(keptPixels, pixels(box, dpi)), "implicit complement must not consume a kept pixel")
			}

			redact := decision(2, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{{
				Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 150, Y0: 101, X1: 198, Y1: 132,
			}}})
			_, err = redaction.Resolve(m, "keep_selected", []redaction.Decision{keep, redact}, recipe)
			requireProblem(t, err, "decision_conflict")
		})
	}
}

func TestResolutionIsIndependentOfDecisionOrder(t *testing.T) {
	m := redactiontest.Map("ABCDE")
	decisions := []redaction.Decision{
		decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}}),
		decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 4, End: 5}}),
	}
	first, err := redaction.Resolve(m, "redact_selected", decisions, testRecipe())
	require.NoError(t, err)
	slices.Reverse(decisions)
	second, err := redaction.Resolve(m, "redact_selected", decisions, testRecipe())
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestSanitizedTextPropertyRetainsOnlySelectedSourceBytes(t *testing.T) {
	for length := 1; length <= 20; length++ {
		source := strings.Repeat("X", length)
		m := redactiontest.Map(source)
		for start := range length {
			for end := start + 1; end <= length; end++ {
				keep := decision(1, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: int64(start), End: int64(end)}})
				plan, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{keep}, testRecipe())
				require.NoError(t, err, "length=%d start=%d end=%d", length, start, end)
				text, err := redaction.Text(plan)
				require.NoError(t, err, "length=%d start=%d end=%d", length, start, end)
				want := ""
				if start > 0 {
					want += "[REDACTED]"
				}
				want += source[start:end]
				if end < length {
					want += "[REDACTED]"
				}
				require.Equal(t, want+"\f", string(text), "length=%d start=%d end=%d", length, start, end)
				for _, run := range plan.Runs {
					if run.Kind == "text" {
						require.Equal(t, source[run.SourceSpan.Start:run.SourceSpan.End], run.Text)
					} else {
						require.Equal(t, "[REDACTED]", run.Text)
					}
				}
			}
		}
	}
}

func TestResolverRejectsInvalidModePrecisely(t *testing.T) {
	_, err := redaction.Resolve(redactiontest.Map("A"), "invalid", nil, testRecipe())
	requireProblem(t, err, "invalid_mode")
}

func TestResolverRejectsPageAndRawMapLimitsBeforeDerivedAllocation(t *testing.T) {
	m := redactiontest.Map("A")
	m.Pages[0].Width = 1_000_000
	m = sealMap(t, m)
	_, err := redaction.Resolve(m, "redact_selected", nil, testRecipe())
	requireProblem(t, err, "render_limit")

	tooManyPages := m
	tooManyPages.Pages = make([]redaction.Page, 1_001)
	_, err = redaction.Resolve(tooManyPages, "redact_selected", nil, testRecipe())
	requireProblem(t, err, "render_limit")

	_, err = redaction.Resolve(redactiontest.Map("A"), "redact_selected", nil,
		redaction.Recipe{DPI: 300, PaddingPixels: int(^uint(0) >> 1)})
	requireProblem(t, err, "render_limit")
}

func TestResolverBoundsSemanticAtomSpanWorkBeforeNormalization(t *testing.T) {
	const count = 7100
	frame := strings.Repeat("d", 64)
	m := redaction.TextMap{Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Text: strings.Repeat("A", count), Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: count * 1000, Height: 1000, Span: redaction.Span{Start: 0, End: count}}},
		Atoms: make([]redaction.Atom, count), Units: []redaction.Unit{{ID: "pathological", Kind: "paragraph", Spans: make([]redaction.Span, count)}}}
	for index := range m.Atoms {
		span := redaction.Span{Start: int64(index), End: int64(index + 1)}
		m.Atoms[index] = redaction.Atom{Span: span, Boxes: []redaction.Box{{
			Page: 1, FrameSHA256: frame, X0: int64(index * 1000), X1: int64(index*1000 + 100), Y1: 100,
		}}}
		m.Units[0].Spans[index] = span
	}
	_, err := redaction.Resolve(m, "redact_selected", nil, testRecipe())
	requireProblem(t, err, "render_limit")
}

func TestResolverBoundsFragmentedKeepComplement(t *testing.T) {
	const count = 5001
	frame := strings.Repeat("d", 64)
	m := redaction.TextMap{Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Text: strings.Repeat("A", count), Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: count * 1000, Height: 1000, Span: redaction.Span{Start: 0, End: count}}},
		Atoms: make([]redaction.Atom, count), Units: []redaction.Unit{{ID: "all", Kind: "paragraph", Spans: []redaction.Span{{Start: 0, End: count}}}}}
	for index := range count {
		m.Atoms[index] = redaction.Atom{Span: redaction.Span{Start: int64(index), End: int64(index + 1)}, Boxes: []redaction.Box{{
			Page: 1, FrameSHA256: frame, X0: int64(index*1000 + 100), X1: int64(index*1000 + 200), Y1: 100,
		}}}
	}
	m = sealMap(t, m)
	keep := decision(1, "keep", redaction.Selector{Kind: "paragraph", MapSHA256: m.SHA256, UnitID: "all"})
	_, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{keep}, testRecipe())
	requireProblem(t, err, "render_limit")
}

func TestResolverBoundsDecisionComparisonWork(t *testing.T) {
	m := redactiontest.Map("A")
	decisions := make([]redaction.Decision, 7072)
	for index := range decisions {
		decisions[index] = decision(index+1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
		decisions[index].Label = "SAME"
	}
	_, err := redaction.Resolve(m, "redact_selected", decisions, testRecipe())
	requireProblem(t, err, "render_limit")
}

func TestResolverSharesWorkBudgetAcrossRepeatedClosures(t *testing.T) {
	const count = 500
	frame := strings.Repeat("d", 64)
	m := redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Text: strings.Repeat("A", count), Pages: []redaction.Page{{
			Number: 1, FrameSHA256: frame, Width: count*100 + 1, Height: 1000, Span: redaction.Span{Start: 0, End: count},
		}}, Atoms: make([]redaction.Atom, count),
	}
	for index := range count {
		m.Atoms[index] = redaction.Atom{Span: redaction.Span{Start: int64(index), End: int64(index + 1)}, Boxes: []redaction.Box{{
			Page: 1, FrameSHA256: frame, X0: int64(index * 100), X1: int64(index*100 + 101), Y1: 100,
		}}}
	}
	m = sealMap(t, m)
	selector := redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: count - 1, End: count}}

	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{
		decision(1, "keep", selector), decision(2, "keep", selector),
	}, testRecipe())
	requireProblem(t, err, "render_limit")
}

func TestResolverSharesWorkBudgetAcrossRepeatedRectangleSweeps(t *testing.T) {
	// Each sweep performs count * (2*count-1) rectangle comparisons:
	// 25,916,400 fits the operation budget, but two sweeps exceed it.
	const count = 3600
	frame := strings.Repeat("d", 64)
	m := sealMap(t, redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: count*100 + 100, Height: 1000}},
	})
	boxes := make([]redaction.Box, count)
	for index := range count {
		boxes[index] = redaction.Box{
			Page: 1, FrameSHA256: frame, X0: int64(index * 100), X1: int64(index*100 + 50), Y1: 100,
		}
	}
	selector := redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: boxes}
	first := decision(1, "keep", selector)
	second := decision(2, "keep", selector)

	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{first}, testRecipe())
	require.NoError(t, err, "one rectangle sweep remains within the operation budget")

	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{first, second}, testRecipe())
	requireProblem(t, err, "render_limit")
}

func TestResolverAcceptsMultiThousandAtomSelections(t *testing.T) {
	const columns, rows = 80, 50
	const count = columns * rows
	frame := strings.Repeat("d", 64)
	m := redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64),
		Text: strings.Repeat("A", count), Pages: []redaction.Page{{
			Number: 1, FrameSHA256: frame, Width: columns * 500, Height: rows * 500, Span: redaction.Span{Start: 0, End: count},
		}}, Atoms: make([]redaction.Atom, count),
	}
	for index := range count {
		x, y := int64(index%columns*500), int64(index/columns*500)
		m.Atoms[index] = redaction.Atom{Span: redaction.Span{Start: int64(index), End: int64(index + 1)}, Boxes: []redaction.Box{{
			Page: 1, FrameSHA256: frame, X0: x, Y0: y, X1: x + 500, Y1: y + 500,
		}}}
	}
	m = sealMap(t, m)
	for _, selector := range []redaction.Selector{
		{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: count}},
		{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}},
	} {
		for _, mode := range []string{"keep_selected", "redact_selected"} {
			t.Run(selector.Kind+"/"+mode, func(t *testing.T) {
				plan, err := redaction.Resolve(m, mode, []redaction.Decision{decision(1, "keep", selector)}, testRecipe())
				require.NoError(t, err, "an ordinary 4,000-atom keep must fit the operation budget")
				text, err := redaction.Text(plan)
				require.NoError(t, err)
				require.Equal(t, strings.Repeat("A", count)+"\f", string(text))
				require.Empty(t, plan.Removed)
				require.Empty(t, plan.RedactBoxes)
			})
		}
	}
}

func TestResolverRejectsIncompleteOrNonCatalogRecipe(t *testing.T) {
	qualified := testRecipe()
	for name, mutate := range map[string]func(*redaction.Recipe){
		"contract":        func(r *redaction.Recipe) { r.Contract = "" },
		"renderer":        func(r *redaction.Recipe) { r.RendererSHA256 = strings.Repeat("f", 64) },
		"writer":          func(r *redaction.Recipe) { r.WriterVersion = "" },
		"font":            func(r *redaction.Recipe) { r.FontSHA256 = "" },
		"padding":         func(r *redaction.Recipe) { r.PaddingPixels-- },
		"pixels":          func(r *redaction.Recipe) { r.MaxPixels = 0 },
		"axis":            func(r *redaction.Recipe) { r.MaxAxis = 0 },
		"smaller axis":    func(r *redaction.Recipe) { r.MaxAxis-- },
		"memory":          func(r *redaction.Recipe) { r.WASMMemoryBytes = 0 },
		"timeout":         func(r *redaction.Recipe) { r.PageTimeoutSeconds = 0 },
		"rss":             func(r *redaction.Recipe) { r.QualifiedPeakRSSBytes = 0 },
		"staging":         func(r *redaction.Recipe) { r.MaxStagingBytes = 0 },
		"unqualified dpi": func(r *redaction.Recipe) { r.DPI = 301 },
	} {
		t.Run(name, func(t *testing.T) {
			recipe := qualified
			mutate(&recipe)
			_, err := redaction.Resolve(redactiontest.Map("A"), "redact_selected", nil, recipe)
			requireProblem(t, err, "render_limit")
		})
	}
}

func TestPartiallyCoveredGapFailsClosed(t *testing.T) {
	m := redactiontest.Map("A")
	m.Pages[0].Width, m.Pages[0].Height = 10_000, 10_000
	gap := redaction.Box{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 1000, Y0: 1000, X1: 9000, Y1: 9000}
	m.Gaps = []redaction.Gap{{Box: gap, Anchor: 0}}
	m = sealMap(t, m)
	partial := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{{
		Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 1000, Y0: 1000, X1: 5000, Y1: 9000,
	}}})
	_, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{partial}, testRecipe())
	requireProblem(t, err, "mapping_incomplete")
}

func TestResolverUsesUTF8ByteBoundaries(t *testing.T) {
	frame := strings.Repeat("c", 64)
	m := redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), Text: "AéB",
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: 3000, Height: 1000, Span: redaction.Span{Start: 0, End: 4}}},
		Atoms: []redaction.Atom{
			{Span: redaction.Span{Start: 0, End: 1}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X1: 100, Y1: 100}}},
			{Span: redaction.Span{Start: 1, End: 3}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: 1000, X1: 1100, Y1: 100}}},
			{Span: redaction.Span{Start: 3, End: 4}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: 2000, X1: 2100, Y1: 100}}},
		},
	}
	m = sealMap(t, m)
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{
		decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 3}}),
	}, testRecipe())
	require.NoError(t, err)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "A[REDACTED]B\f", string(text))

	broken := decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{broken}, testRecipe())
	require.Error(t, err)
}

func TestExplicitPaddingIsAppliedOnceBeforeClosure(t *testing.T) {
	recipe, err := pdfproduction.QualifiedRecipeForDPI(300)
	require.NoError(t, err)
	m := redactiontest.Map("A")
	m.Pages[0].Width, m.Pages[0].Height = 1000, 1000
	m.Atoms[0].Boxes[0] = redaction.Box{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 300, Y0: 300, X1: 400, Y1: 400}
	m = sealMap(t, m)
	seed := redaction.Box{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256, X0: 200, Y0: 300, X1: 267, Y1: 400}
	redact := decision(1, "redact", redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{seed}})
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{redact}, recipe)
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "selection_expansion_required", problem.Code)
	for _, box := range problem.ExpandedBoxes {
		require.LessOrEqual(t, pixels(box, 300).x1, int64(12), "closure footprints are reported without a second pad")
	}
	redact.Selector = *problem.Expanded
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{redact}, recipe)
	require.NoError(t, err)
	// The accepted expansion is now the explicit seed and receives exactly one
	// two-pixel pad; it ends at pixel 14, never 15.
	for _, box := range plan.RedactBoxes {
		got := pixels(box, 300)
		require.LessOrEqual(t, got.x1, int64(14), "explicit expansion was padded more than once: %+v", got)
	}
}

func TestKeepSelectedRejectsClosurePaddingIntoRetainedAtom(t *testing.T) {
	m := redactiontest.Map("ABC")
	m.Pages[0].Width, m.Pages[0].Height = 1000, 1000
	for index, bounds := range [][2]int64{{100, 200}, {233, 333}, {367, 467}} {
		m.Atoms[index].Boxes[0] = redaction.Box{Page: 1, FrameSHA256: m.Pages[0].FrameSHA256,
			X0: bounds[0], X1: bounds[1], Y0: 300, Y1: 400}
	}
	m = sealMap(t, m)
	// A's padding selects B. B's padding reaches C, so keeping C must fail
	// instead of silently shortening the padding around the removed B.
	_, err := redaction.Resolve(m, "keep_selected", []redaction.Decision{
		decision(1, "keep", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 2, End: 3}}),
		decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}}),
	}, testRecipe())
	requireProblem(t, err, "decision_conflict")
}

func decision(index int, action string, selector redaction.Selector) redaction.Decision {
	return redaction.Decision{
		ID:       fmt.Sprintf("%08x-0000-4000-8000-%012x", index, index),
		MemberID: "22222222-2222-4222-8222-222222222222", Action: action, Selector: selector,
	}
}

func testRecipe() redaction.Recipe {
	recipe, err := pdfproduction.QualifiedRecipeForDPI(300)
	if err != nil {
		panic(err)
	}
	return recipe
}

func sealMap(t *testing.T, m redaction.TextMap) redaction.TextMap {
	t.Helper()
	m = redaction.NormalizeTextMap(m)
	_, digest, err := redaction.CanonicalTextMap(m)
	require.NoError(t, err)
	m.SHA256 = digest
	require.NoError(t, redaction.ValidateMap(m))
	return m
}

func sealMapWithoutValidation(t *testing.T, m redaction.TextMap) redaction.TextMap {
	t.Helper()
	m = redaction.NormalizeTextMap(m)
	_, digest, err := redaction.CanonicalTextMap(m)
	require.NoError(t, err)
	m.SHA256 = digest
	return m
}

func pagedMap(t *testing.T, text string, boundaries []int) redaction.TextMap {
	t.Helper()
	m := redaction.TextMap{Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), Text: text}
	for pageIndex := 0; pageIndex+1 < len(boundaries); pageIndex++ {
		start, end := boundaries[pageIndex], boundaries[pageIndex+1]
		frame := fmt.Sprintf("%064x", pageIndex+10)
		m.Pages = append(m.Pages, redaction.Page{Number: pageIndex + 1, FrameSHA256: frame, Width: int64(max(1, end-start) * 1000), Height: 1000, Span: redaction.Span{Start: int64(start), End: int64(end)}})
		for offset := start; offset < end; offset++ {
			x := int64((offset - start) * 1000)
			m.Atoms = append(m.Atoms, redaction.Atom{Span: redaction.Span{Start: int64(offset), End: int64(offset + 1)}, Boxes: []redaction.Box{{
				Page: pageIndex + 1, FrameSHA256: frame, X0: x, X1: x + 100, Y1: 100,
			}}})
		}
	}
	return sealMap(t, m)
}

func requireProblem(t *testing.T, err error, code string) {
	t.Helper()
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem, "error %v is not a redaction problem", err)
	require.Equal(t, code, problem.Code)
}

type pixelBox struct{ x0, y0, x1, y1 int64 }

func pixels(box redaction.Box, dpi int) pixelBox {
	d := int64(dpi)
	return pixelBox{box.X0 * d / 10_000, box.Y0 * d / 10_000, (box.X1*d + 9_999) / 10_000, (box.Y1*d + 9_999) / 10_000}
}

func overlaps(a, b pixelBox) bool { return a.x0 < b.x1 && a.x1 > b.x0 && a.y0 < b.y1 && a.y1 > b.y0 }
