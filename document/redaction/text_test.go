package redaction_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestTextRejectsTamperedOrStructurallyInvalidPlans(t *testing.T) {
	m := redactiontest.Map("ABC")
	decision := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
	base, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{decision}, testRecipe())
	require.NoError(t, err)

	tests := map[string]func(*redaction.Resolved){
		"unbound mutation": func(plan *redaction.Resolved) { plan.Runs[0].Text = "Z" },
		"unknown run kind": func(plan *redaction.Resolved) { plan.Runs[0].Kind = "private"; rehash(t, plan) },
		"arbitrary marker": func(plan *redaction.Resolved) {
			plan.Runs[1].Text = "[SECRET]"
			rehash(t, plan)
		},
		"marker source span": func(plan *redaction.Resolved) {
			plan.Runs[1].SourceSpan = &redaction.Span{Start: 1, End: 2}
			rehash(t, plan)
		},
		"overlapping source spans": func(plan *redaction.Resolved) {
			plan.Runs[2].SourceSpan = &redaction.Span{Start: 0, End: 1}
			rehash(t, plan)
		},
		"run outside page inventory": func(plan *redaction.Resolved) {
			plan.Runs[0].Page = 2
			rehash(t, plan)
		},
		"box on another page": func(plan *redaction.Resolved) {
			plan.Runs[0].Boxes[0].Page = 2
			rehash(t, plan)
		},
		"missing page count": func(plan *redaction.Resolved) { plan.PageCount = 0; rehash(t, plan) },
		"nil canonical runs": func(plan *redaction.Resolved) { plan.Runs = nil; rehash(t, plan) },
		"unordered removals": func(plan *redaction.Resolved) {
			plan.Removed = []redaction.Span{{Start: 2, End: 3}, {Start: 0, End: 1}}
			rehash(t, plan)
		},
		"invalid uncertain decision": func(plan *redaction.Resolved) {
			plan.UncertainDecisionIDs = []string{"not-a-uuid"}
			rehash(t, plan)
		},
		"empty public region label": func(plan *redaction.Resolved) {
			plan.Regions[0].Label = ""
			rehash(t, plan)
		},
		"invalid mask box": func(plan *redaction.Resolved) {
			plan.RedactBoxes[0].X1 = plan.RedactBoxes[0].X0
			rehash(t, plan)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			plan := clonePlan(base)
			mutate(&plan)
			_, err := redaction.Text(plan)
			require.Error(t, err)
		})
	}
}

func TestTextEmitsLeadingMiddleAndTrailingBlankPages(t *testing.T) {
	m := pagedMap(t, "A", []int{0, 0, 1, 1, 1})
	plan, err := redaction.Resolve(m, "redact_selected", nil, testRecipe())
	require.NoError(t, err)
	require.Equal(t, 4, plan.PageCount)
	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "\fA\f\f\f", string(text))
}

func TestTextAcceptsCanonicalMaskUnionThatCoversUnorderedPage(t *testing.T) {
	m := redactiontest.Map("A")
	whole := decision(1, "redact", redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}})
	plan, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{whole}, testRecipe())
	require.NoError(t, err)
	page := plan.Pages[0]
	halfX, halfY := page.Width/2, page.Height/2
	require.Positive(t, halfX)
	require.Positive(t, halfY)
	plan.RedactBoxes = []redaction.Box{
		{Page: page.Number, FrameSHA256: page.FrameSHA256, X1: halfX, Y1: page.Height},
		// Adjacent raster strips can round-trip to physical box coordinates
		// separated by one 1/100-point unit while sharing the same pixel edge.
		{Page: page.Number, FrameSHA256: page.FrameSHA256, X0: halfX + 1, X1: page.Width, Y1: halfY},
		{Page: page.Number, FrameSHA256: page.FrameSHA256, X0: halfX + 1, Y0: halfY, X1: page.Width, Y1: page.Height},
	}
	plan.Gaps = []redaction.Gap{{Box: plan.Runs[0].Boxes[0], Anchor: page.Span.Start, Unordered: true}}
	rehash(t, &plan)

	text, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]\f", string(text))
}

func TestTextRejectsRehashedRemovedAndMarkerContradictions(t *testing.T) {
	m := redactiontest.Map("ABC")
	redact := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2}})
	base, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{redact}, testRecipe())
	require.NoError(t, err)
	require.Equal(t, []string{"text", "redaction", "text"}, []string{base.Runs[0].Kind, base.Runs[1].Kind, base.Runs[2].Kind})

	for name, mutate := range map[string]func(*redaction.Resolved){
		"source overlaps removed": func(plan *redaction.Resolved) {
			plan.Runs[0].Text = "X"
			plan.Runs[0].SourceSpan = &redaction.Span{Start: 1, End: 2}
		},
		"missing marker": func(plan *redaction.Resolved) {
			plan.Runs = append(plan.Runs[:1], plan.Runs[2:]...)
		},
		"surplus marker": func(plan *redaction.Resolved) {
			plan.Runs = slices.Insert(plan.Runs, 2, plan.Runs[1])
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := clonePlan(base)
			mutate(&plan)
			rehash(t, &plan)
			_, err := redaction.Text(plan)
			require.Error(t, err)
		})
	}

	wide := redactiontest.Map("ABCD")
	wideRedact := decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: wide.SHA256, Span: &redaction.Span{Start: 1, End: 3}})
	split, err := redaction.Resolve(wide, "redact_selected", []redaction.Decision{wideRedact}, testRecipe())
	require.NoError(t, err)
	require.Len(t, split.Runs, 3)
	split.Removed = []redaction.Span{{Start: 1, End: 2}, {Start: 2, End: 3}}
	split.Runs = slices.Insert(split.Runs, 2, split.Runs[1])
	rehash(t, &split)
	_, err = redaction.Text(split)
	require.Error(t, err, "one maximal removed page interval cannot be represented by split markers")
}

func TestTextRejectsRehashedNoncanonicalResolvedCollections(t *testing.T) {
	m := redactiontest.Map("ABC")
	left := decision(1, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 0, End: 1}})
	right := decision(2, "redact", redaction.Selector{Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 2, End: 3}})
	left.Label, right.Label = "LEFT", "RIGHT"
	base, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{left, right}, testRecipe())
	require.NoError(t, err)
	require.Len(t, base.RedactBoxes, 2)
	require.Len(t, base.Regions, 2)

	for name, mutate := range map[string]func(*redaction.Resolved){
		"run permutation":  func(plan *redaction.Resolved) { slices.Reverse(plan.Runs) },
		"mask permutation": func(plan *redaction.Resolved) { slices.Reverse(plan.RedactBoxes) },
		"duplicate mask": func(plan *redaction.Resolved) {
			plan.RedactBoxes = append(plan.RedactBoxes, plan.RedactBoxes[0])
		},
		"duplicate run box": func(plan *redaction.Resolved) {
			plan.Runs[0].Boxes = append(plan.Runs[0].Boxes, plan.Runs[0].Boxes[0])
		},
		"region permutation": func(plan *redaction.Resolved) { slices.Reverse(plan.Regions) },
		"duplicate region box": func(plan *redaction.Resolved) {
			plan.Regions[0].Boxes = append(plan.Regions[0].Boxes, plan.Regions[0].Boxes[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := clonePlan(base)
			mutate(&plan)
			rehash(t, &plan)
			_, err := redaction.Text(plan)
			require.Error(t, err)
		})
	}

	adjacent, err := redaction.Resolve(redactiontest.Map("ABCD"), "redact_selected", []redaction.Decision{
		decision(3, "redact", redaction.Selector{Kind: "text", MapSHA256: redactiontest.Map("ABCD").SHA256, Span: &redaction.Span{Start: 1, End: 3}}),
	}, testRecipe())
	require.NoError(t, err)
	adjacent.Removed = []redaction.Span{{Start: 1, End: 2}, {Start: 2, End: 3}}
	rehash(t, &adjacent)
	_, err = redaction.Text(adjacent)
	require.Error(t, err, "adjacent removed spans are not the maximal canonical union")
}

func TestTextReturnsTypedLimitsForPageRunsAndRegions(t *testing.T) {
	m := redactiontest.Map("A")
	whole := decision(1, "redact", redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}})
	base, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{whole}, testRecipe())
	require.NoError(t, err)

	t.Run("page runs", func(t *testing.T) {
		plan := clonePlan(base)
		plan.Runs = make([]redaction.Run, 16_385)
		for index := range plan.Runs {
			plan.Runs[index] = base.Runs[0]
		}
		rehash(t, &plan)
		_, err := redaction.Text(plan)
		requireProblem(t, err, "render_limit")
	})

	t.Run("regions", func(t *testing.T) {
		plan := clonePlan(base)
		plan.Regions = make([]redaction.RedactionRegion, 1_000_001)
		_, err := redaction.Text(plan)
		requireProblem(t, err, "render_limit")
	})
}

func TestReviewBindingTracksEveryAuthorityChangeButNotDeclarationFields(t *testing.T) {
	base := reviewInput()
	want, err := redaction.ReviewBinding(base)
	require.NoError(t, err)

	mutations := map[string]func(*redaction.ReviewInput){
		"decision add":        func(v *redaction.ReviewInput) { v.DecisionsSHA256 = digestString("decisions plus one") },
		"decision remove":     func(v *redaction.ReviewInput) { v.DecisionsSHA256 = digestString("decisions minus one") },
		"decision replace":    func(v *redaction.ReviewInput) { v.DecisionsSHA256 = digestString("replacement decision") },
		"resolved plan":       func(v *redaction.ReviewInput) { v.ResolvedSHA256 = digestString("new resolved plan") },
		"membership order":    func(v *redaction.ReviewInput) { v.Ordinal++; v.MemberHash = digestString("reordered members") },
		"instructions":        func(v *redaction.ReviewInput) { v.InstructionsSHA256 = digestString("new instructions") },
		"recipe":              func(v *redaction.ReviewInput) { v.RecipeSHA256 = digestString("new recipe") },
		"mode":                func(v *redaction.ReviewInput) { v.Mode = "redact_selected" },
		"source version":      func(v *redaction.ReviewInput) { v.SourceVersionID = "55555555-5555-4555-8555-555555555555" },
		"source bytes":        func(v *redaction.ReviewInput) { v.SourceSHA256 = digestString("new source"); v.SourceSize++ },
		"PDF rendition":       func(v *redaction.ReviewInput) { v.PDFSHA256 = digestString("new PDF"); v.PDFSize++ },
		"page inventory":      func(v *redaction.ReviewInput) { v.PageInventorySHA256 = digestString("new inventory") },
		"aligned map":         func(v *redaction.ReviewInput) { v.MapSHA256 = digestString("new map") },
		"production revision": func(v *redaction.ReviewInput) { v.Revision++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			got, err := redaction.ReviewBinding(changed)
			require.NoError(t, err)
			require.NotEqual(t, want, got)
		})
	}

	member := redaction.Member{Reviewed: false, ReviewBinding: ""}
	member.Reviewed, member.ReviewBinding = true, strings.Repeat("f", 64)
	got, err := redaction.ReviewBinding(base)
	require.NoError(t, err)
	require.Equal(t, want, got, "declaration-only member fields are not ReviewInput")
}

func TestReviewBindingRejectsMalformedClaims(t *testing.T) {
	for name, mutate := range map[string]func(*redaction.ReviewInput){
		"set":      func(v *redaction.ReviewInput) { v.SetID = "not-a-uuid" },
		"digest":   func(v *redaction.ReviewInput) { v.ResolvedSHA256 = strings.Repeat("A", 64) },
		"revision": func(v *redaction.ReviewInput) { v.Revision = 0 },
		"mode":     func(v *redaction.ReviewInput) { v.Mode = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			input := reviewInput()
			mutate(&input)
			_, err := redaction.ReviewBinding(input)
			require.Error(t, err)
		})
	}
}

func reviewInput() redaction.ReviewInput {
	return redaction.ReviewInput{
		SetID: "11111111-1111-4111-8111-111111111111", MemberID: "22222222-2222-4222-8222-222222222222",
		VaultID: "33333333-3333-4333-8333-333333333333", SourceVersionID: "44444444-4444-4444-8444-444444444444",
		Revision: 2, Ordinal: 4, NodeID: 9, SourceSize: 10, PDFSize: 11,
		SourceSHA256: digestString("source"), PDFSHA256: digestString("pdf"), PageInventorySHA256: digestString("pages"),
		MapSHA256: digestString("map"), Mode: "keep_selected", MemberHash: digestString("members"),
		InstructionsSHA256: digestString("instructions"), RecipeSHA256: digestString("recipe"),
		DecisionsSHA256: digestString("decisions"), ResolvedSHA256: digestString("resolved"),
	}
}

func digestString(value string) string {
	// These fixed-width lowercase values are independent test identities; their
	// cryptographic preimages are irrelevant to ReviewBinding validation.
	encoded := fmtDigest(value)
	return encoded[:64]
}

func fmtDigest(value string) string {
	const alphabet = "0123456789abcdef"
	var result strings.Builder
	for index := range 64 {
		result.WriteByte(alphabet[(int(value[index%len(value)])+index)%len(alphabet)])
	}
	return result.String()
}

func clonePlan(plan redaction.Resolved) redaction.Resolved {
	result := plan
	result.Pages = slices.Clone(plan.Pages)
	result.Gaps = slices.Clone(plan.Gaps)
	result.RedactBoxes = slices.Clone(plan.RedactBoxes)
	result.Removed = slices.Clone(plan.Removed)
	result.UncertainDecisionIDs = slices.Clone(plan.UncertainDecisionIDs)
	result.Regions = make([]redaction.RedactionRegion, len(plan.Regions))
	for index, region := range plan.Regions {
		result.Regions[index] = region
		result.Regions[index].Boxes = slices.Clone(region.Boxes)
		result.Regions[index].DecisionIDs = slices.Clone(region.DecisionIDs)
	}
	result.Runs = make([]redaction.Run, len(plan.Runs))
	for index, run := range plan.Runs {
		result.Runs[index] = run
		result.Runs[index].Boxes = slices.Clone(run.Boxes)
		if run.SourceSpan != nil {
			span := *run.SourceSpan
			result.Runs[index].SourceSpan = &span
		}
	}
	return result
}

func rehash(t *testing.T, plan *redaction.Resolved) {
	t.Helper()
	_, digest, err := redaction.CanonicalResolved(*plan)
	require.NoError(t, err)
	plan.SHA256 = digest
}
