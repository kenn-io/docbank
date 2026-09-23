package production

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func TestPlanEndorsementPagesUsesMaskAtEightPointsThenOrderedStrip(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	resolved := endorsementResolved(t, recipe, []redaction.Page{
		{Number: 1, FrameSHA256: endorsementTestHash("portrait crop"), Width: 10_000, Height: 14_000, Span: redaction.Span{Start: 0, End: 1}},
		{Number: 2, FrameSHA256: endorsementTestHash("rotated landscape crop"), Width: 16_000, Height: 8_000, Span: redaction.Span{Start: 1, End: 2}},
	}, []endorsementDecision{
		{id: "11111111-1111-4111-8111-111111111111", label: "NOTICE", page: 1, x0: 500, y0: 2_000, x1: 7_500, y1: 5_000},
		{id: "22222222-2222-4222-8222-222222222222", label: "CONFIDENTIAL", page: 1, x0: 500, y0: 6_000, x1: 700, y1: 6_200},
		{id: "33333333-3333-4333-8333-333333333333", label: "PUBLIC", page: 2, x0: 1_000, y0: 1_000, x1: 12_000, y1: 4_000},
	})
	memberID := "44444444-4444-4444-8444-444444444444"
	numbers := []documentproduction.AssignedNumber{
		{MemberID: memberID, MemberOrdinal: 7, Page: 2, Text: "SYN000002"},
		{MemberID: memberID, MemberOrdinal: 7, Page: 1, Text: "SYN000001"},
	}

	pages, err := PlanEndorsementPages(memberID, resolved.Pages, resolved, numbers, recipe)
	require.NoError(t, err)
	require.Len(t, pages, 2)
	require.Equal(t, []string{"NOTICE", "CONFIDENTIAL", "SYN000001"}, endorsementTexts(pages[0].Endorsements))
	require.Equal(t, []string{"PUBLIC", "SYN000002"}, endorsementTexts(pages[1].Endorsements))

	for _, page := range pages {
		require.Equal(t, int64(5_000), page.Layout.StripHeight)
		require.Equal(t, page.Layout.Source.Height+5_000, page.Layout.Output.Height)
		require.Equal(t, page.Layout.Source.Width, page.Layout.Output.Width)
		require.NotEqual(t, page.Layout.Source.FrameSHA256, page.Layout.Output.FrameSHA256)
		require.NotEmpty(t, page.SHA256)
		for _, endorsement := range page.Endorsements {
			require.Equal(t, int64(8_000), endorsement.FontSizeMilliPoints)
			require.Equal(t, recipe.FontSHA256, endorsement.FontSHA256)
			require.Equal(t, page.Layout.Output.FrameSHA256, endorsement.Box.FrameSHA256)
		}
	}
	require.Equal(t, "label", pages[0].Endorsements[0].Kind)
	require.LessOrEqual(t, pages[0].Endorsements[0].Box.Y1, pages[0].Layout.Source.Height)
	require.Equal(t, "legend", pages[0].Endorsements[1].Kind)
	require.GreaterOrEqual(t, pages[0].Endorsements[1].Box.Y0, pages[0].Layout.Source.Height)
	require.Equal(t, "number", pages[0].Endorsements[2].Kind)
	require.GreaterOrEqual(t, pages[0].Endorsements[2].Box.Y0, pages[0].Layout.Source.Height)

	reordered := []documentproduction.AssignedNumber{numbers[1], numbers[0]}
	again, err := PlanEndorsementPages(memberID, resolved.Pages, resolved, reordered, recipe)
	require.NoError(t, err)
	require.Equal(t, pages, again)

	requireWritesEndorsedPages(t, pages, resolved, recipe)
}

func TestPlanEndorsementPagesWrapsNarrowLegendsWithoutOverlap(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	resolved := endorsementResolved(t, recipe, []redaction.Page{{Number: 1, FrameSHA256: endorsementTestHash("wrap"), Width: 12_000, Height: 12_000, Span: redaction.Span{Start: 0, End: 1}}}, []endorsementDecision{{id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", label: "ALPHA", page: 1, x0: 500, y0: 500, x1: 2_000, y1: 1_000}, {id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", label: "BETA", page: 1, x0: 2_500, y0: 500, x1: 4_000, y1: 1_000}, {id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", label: "GAMMA", page: 1, x0: 4_500, y0: 500, x1: 6_000, y1: 1_000}, {id: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", label: "DELTA", page: 1, x0: 6_500, y0: 500, x1: 8_000, y1: 1_000}})
	memberID := "44444444-4444-4444-8444-444444444444"
	pages, err := PlanEndorsementPages(memberID, resolved.Pages, resolved, []documentproduction.AssignedNumber{{MemberID: memberID, MemberOrdinal: 1, Page: 1, Text: "B000001"}}, recipe)
	require.NoError(t, err)
	require.Len(t, pages, 1)
	var strip []redaction.Endorsement
	for _, endorsement := range pages[0].Endorsements {
		if endorsement.Box.Y0 >= pages[0].Layout.Source.Height {
			strip = append(strip, endorsement)
		}
	}
	require.NotEmpty(t, strip)
	rows := map[int64]bool{}
	for _, endorsement := range strip {
		rows[endorsement.Box.Y0] = true
		require.LessOrEqual(t, endorsement.Box.Y1, pages[0].Layout.Output.Height)
	}
	require.GreaterOrEqual(t, len(rows), 2)
	var laterY, laterX int64
	for y := range rows {
		if y > laterY {
			laterY = y
		}
	}
	laterX = 1 << 62
	for _, endorsement := range strip {
		if endorsement.Box.Y0 == laterY && endorsement.Box.X0 < laterX {
			laterX = endorsement.Box.X0
		}
	}
	require.LessOrEqual(t, laterX, int64(1_000))
	for i := range strip {
		for j := i + 1; j < len(strip); j++ {
			overlap := strip[i].Box.X0 < strip[j].Box.X1 && strip[j].Box.X0 < strip[i].Box.X1 && strip[i].Box.Y0 < strip[j].Box.Y1 && strip[j].Box.Y0 < strip[i].Box.Y1
			require.False(t, overlap)
		}
	}
	requireWritesEndorsedPages(t, pages, resolved, recipe)
}

func TestPlanEndorsementPagesFallsBackWhenMaskLabelsWouldConflict(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	base := endorsementResolved(t, recipe, []redaction.Page{{
		Number: 1, FrameSHA256: endorsementTestHash("label geometry"), Width: 16_000, Height: 16_000, Span: redaction.Span{Start: 0, End: 1},
	}}, []endorsementDecision{{
		id: "11111111-1111-4111-8111-111111111111", label: "FIRST", page: 1, x0: 1_000, y0: 1_000, x1: 12_000, y1: 12_000,
	}})
	for name, test := range map[string]struct {
		mutate    func(redaction.Resolved) redaction.Resolved
		wantKinds []string
	}{
		"label outside one mask": {
			mutate: func(resolved redaction.Resolved) redaction.Resolved {
				box := resolved.Regions[0].Boxes[0]
				box.X1 += 1_000
				resolved.Regions = []redaction.RedactionRegion{{
					Boxes: []redaction.Box{box}, Label: "FIRST", DecisionIDs: []string{"11111111-1111-4111-8111-111111111111"},
				}}
				return endorsementRecacheResolved(t, resolved)
			},
			wantKinds: []string{"legend"},
		},
		"overlapping label boxes": {
			mutate: func(resolved redaction.Resolved) redaction.Resolved {
				outer := resolved.RedactBoxes[0]
				inner := outer
				inner.X0 += 1_000
				inner.Y0 += 1_000
				resolved.Regions = []redaction.RedactionRegion{
					{Boxes: []redaction.Box{outer}, Label: "FIRST", DecisionIDs: []string{"11111111-1111-4111-8111-111111111111"}},
					{Boxes: []redaction.Box{inner}, Label: "SECOND", DecisionIDs: []string{"22222222-2222-4222-8222-222222222222"}},
				}
				return endorsementRecacheResolved(t, resolved)
			},
			wantKinds: []string{"label", "legend"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolved := test.mutate(base)
			pages, err := PlanEndorsementPages("44444444-4444-4444-8444-444444444444", resolved.Pages, resolved, nil, recipe)
			require.NoError(t, err)
			require.Len(t, pages, 1)
			require.Equal(t, test.wantKinds, endorsementKinds(pages[0].Endorsements))
			requireWritesEndorsedPages(t, pages, resolved, recipe)
		})
	}
}

func endorsementRecacheResolved(t *testing.T, resolved redaction.Resolved) redaction.Resolved {
	t.Helper()
	_, digest, err := redaction.CanonicalResolved(resolved)
	require.NoError(t, err)
	resolved.SHA256 = digest
	_, err = redaction.Text(resolved)
	require.NoError(t, err)
	return resolved
}

func requireWritesEndorsedPages(t *testing.T, pages []EndorsedPage, resolved redaction.Resolved, recipe redaction.Recipe) {
	t.Helper()
	artifacts := make([]pdfproduction.PageArtifact, 0, len(pages))
	for index, planned := range pages {
		source := planned.Layout.Source
		width := int((source.Width*int64(recipe.DPI) + 9_999) / 10_000)
		height := int((source.Height*int64(recipe.DPI) + 9_999) / 10_000)
		pixels := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		burned, err := pdfproduction.Burn(pdfproduction.Raster{Pixels: pixels, Page: source, DPI: recipe.DPI}, pageBoxes(resolved.RedactBoxes, source.Number), recipe)
		require.NoError(t, err)
		endorsed, err := pdfproduction.Endorse(burned, planned.Layout, planned.Endorsements, recipe)
		require.NoError(t, err)
		expectedHeight := int((planned.Layout.Output.Height*int64(recipe.DPI) + 9_999) / 10_000)
		require.Equal(t, expectedHeight, endorsed.Pixels.Bounds().Dy(), "page %d", index+1)
		var encoded bytes.Buffer
		require.NoError(t, png.Encode(&encoded, endorsed.Pixels))
		pngBytes := bytes.Clone(encoded.Bytes())
		layoutBytes, err := canonical.Marshal(planned.Layout)
		require.NoError(t, err)
		endorsementBytes, err := canonical.Marshal(planned.Endorsements)
		require.NoError(t, err)
		artifacts = append(artifacts, pdfproduction.PageArtifact{
			Page: planned.Layout.Output, PNGSHA256: endorsementTestDigest(pngBytes), PNGSize: int64(len(pngBytes)),
			ResolvedSHA256: resolved.SHA256, Layout: planned.Layout, LayoutSHA256: endorsementTestDigest(layoutBytes),
			Endorsements: planned.Endorsements, EndorsementsSHA256: endorsementTestDigest(endorsementBytes),
			Runs:    pageRuns(resolved.Runs, source.Number),
			OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(pngBytes)), nil },
		})
	}
	var pdf bytes.Buffer
	require.NoError(t, pdfproduction.WriteFresh(t.Context(), &pdf, &endorsementArtifactSequence{pages: artifacts}, resolved, recipe))
}

type endorsementArtifactSequence struct {
	pages []pdfproduction.PageArtifact
	index int
}

func (sequence *endorsementArtifactSequence) Next(context.Context) (pdfproduction.PageArtifact, error) {
	if sequence.index == len(sequence.pages) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	page := sequence.pages[sequence.index]
	sequence.index++
	return page, nil
}

func TestPlanEndorsementPagesRejectsTextAndPageOverflowAsLayoutConflict(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	memberID := "44444444-4444-4444-8444-444444444444"
	for name, test := range map[string]struct {
		resolved redaction.Resolved
		numbers  []documentproduction.AssignedNumber
	}{
		"long public label": {
			resolved: endorsementResolved(t, recipe, []redaction.Page{{
				Number: 1, FrameSHA256: endorsementTestHash("narrow crop"), Width: 10_000, Height: 10_000, Span: redaction.Span{Start: 0, End: 1},
			}}, []endorsementDecision{{
				id: "11111111-1111-4111-8111-111111111111", label: strings.Repeat("W", redaction.MaxDecisionLabelBytes),
				page: 1, x0: 500, y0: 2_000, x1: 700, y1: 2_200,
			}}),
		},
		"strip exceeds qualified axis": {
			resolved: endorsementResolved(t, recipe, []redaction.Page{{
				Number: 1, FrameSHA256: endorsementTestHash("tall crop"), Width: 10_000, Height: 546_000, Span: redaction.Span{Start: 0, End: 1},
			}}, nil),
			numbers: []documentproduction.AssignedNumber{{MemberID: memberID, MemberOrdinal: 1, Page: 1, Text: "SYN000001"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PlanEndorsementPages(memberID, test.resolved.Pages, test.resolved, test.numbers, recipe)
			var problem *redaction.Problem
			require.ErrorAs(t, err, &problem)
			require.Equal(t, "endorsement_layout_conflict", problem.Code)
		})
	}
}

func TestPlanEndorsementPagesRejectsCombinedSourceAndEndorsementTextOverflow(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	resolved := endorsementResolved(t, recipe, []redaction.Page{{
		Number: 1, FrameSHA256: endorsementTestHash("text budget frame"), Width: 10_000, Height: 10_000, Span: redaction.Span{Start: 0, End: 1},
	}}, nil)
	resolved.Pages[0].Span.End = 1 << 20
	resolved.Runs[0].Text = strings.Repeat("A", 1<<20)
	resolved.Runs[0].SourceSpan.End = 1 << 20
	_, digest, err := redaction.CanonicalResolved(resolved)
	require.NoError(t, err)
	resolved.SHA256 = digest
	memberID := "44444444-4444-4444-8444-444444444444"

	_, err = PlanEndorsementPages(memberID, resolved.Pages, resolved, []documentproduction.AssignedNumber{{
		MemberID: memberID, MemberOrdinal: 1, Page: 1, Text: "SYN000001",
	}}, recipe)
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "endorsement_layout_conflict", problem.Code)
}

func TestPlanEndorsementPagesKeepsDuplicateOccurrencesAndPrivateReasonsSeparate(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	privateCanary := "PRIVATE-OWNER-REASON-MUST-NOT-LEAK"
	resolved := endorsementResolved(t, recipe, []redaction.Page{{
		Number: 1, FrameSHA256: endorsementTestHash("duplicate source frame"), Width: 12_000, Height: 10_000, Span: redaction.Span{Start: 0, End: 1},
	}}, []endorsementDecision{{
		id: "11111111-1111-4111-8111-111111111111", label: "PUBLIC LABEL", reason: privateCanary,
		page: 1, x0: 500, y0: 2_000, x1: 9_000, y1: 5_000,
	}})
	firstMember := "44444444-4444-4444-8444-444444444444"
	secondMember := "55555555-5555-4555-8555-555555555555"
	numbers := []documentproduction.AssignedNumber{
		{MemberID: secondMember, MemberOrdinal: 2, Page: 1, Text: "SYN000002"},
		{MemberID: firstMember, MemberOrdinal: 1, Page: 1, Text: "SYN000001"},
	}

	first, err := PlanEndorsementPages(firstMember, resolved.Pages, resolved, numbers, recipe)
	require.NoError(t, err)
	second, err := PlanEndorsementPages(secondMember, resolved.Pages, resolved, numbers, recipe)
	require.NoError(t, err)
	require.Equal(t, []string{"PUBLIC LABEL", "SYN000001"}, endorsementTexts(first[0].Endorsements))
	require.Equal(t, []string{"PUBLIC LABEL", "SYN000002"}, endorsementTexts(second[0].Endorsements))
	require.NotEqual(t, first[0].SHA256, second[0].SHA256)

	firstBytes, err := canonical.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(firstBytes), privateCanary)
	require.Contains(t, string(firstBytes), "PUBLIC LABEL")
}

func TestPlanEndorsementPagesOmitsStripWhenEveryLabelFitsAndNoNumbersReserved(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	resolved := endorsementResolved(t, recipe, []redaction.Page{{
		Number: 1, FrameSHA256: endorsementTestHash("unrotated crop"), Width: 10_000, Height: 10_000, Span: redaction.Span{Start: 0, End: 1},
	}}, []endorsementDecision{{
		id: "11111111-1111-4111-8111-111111111111", label: "PUBLIC", page: 1, x0: 500, y0: 2_000, x1: 9_000, y1: 5_000,
	}})

	pages, err := PlanEndorsementPages("44444444-4444-4444-8444-444444444444", resolved.Pages, resolved, nil, recipe)
	require.NoError(t, err)
	require.Len(t, pages, 1)
	require.Zero(t, pages[0].Layout.StripHeight)
	require.Equal(t, pages[0].Layout.Source, pages[0].Layout.Output)
	require.Equal(t, "label", pages[0].Endorsements[0].Kind)
}

type endorsementDecision struct {
	id, label      string
	reason         string
	page           int
	x0, y0, x1, y1 int64
}

func endorsementResolved(t *testing.T, recipe redaction.Recipe, pages []redaction.Page, decisions []endorsementDecision) redaction.Resolved {
	t.Helper()
	text := make([]byte, len(pages))
	atoms := make([]redaction.Atom, len(pages))
	for index, page := range pages {
		text[index] = byte('A' + index)
		atoms[index] = redaction.Atom{Span: page.Span, Boxes: []redaction.Box{{
			Page: page.Number, FrameSHA256: page.FrameSHA256, X0: 100, Y0: 100, X1: 200, Y1: 300,
		}}}
	}
	m := redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: endorsementTestHash("synthetic PDF"),
		EvidenceSHA256: endorsementTestHash("synthetic evidence"), Text: string(text),
		Pages: pages, Atoms: atoms, Units: []redaction.Unit{}, Gaps: []redaction.Gap{},
	}
	_, digest, err := redaction.CanonicalTextMap(m)
	require.NoError(t, err)
	m.SHA256 = digest
	require.NoError(t, redaction.ValidateMap(m))
	values := make([]redaction.Decision, 0, len(decisions))
	for _, decision := range decisions {
		page := pages[decision.page-1]
		values = append(values, redaction.Decision{
			ID: decision.id, MemberID: "44444444-4444-4444-8444-444444444444", Action: "redact", Label: decision.label, Reason: decision.reason,
			Selector: redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{{
				Page: decision.page, FrameSHA256: page.FrameSHA256,
				X0: decision.x0, Y0: decision.y0, X1: decision.x1, Y1: decision.y1,
			}}},
		})
	}
	resolved, err := redaction.Resolve(m, "redact_selected", values, recipe)
	require.NoError(t, err)
	return resolved
}

func endorsementTexts(values []redaction.Endorsement) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = value.Text
	}
	return result
}

func endorsementKinds(values []redaction.Endorsement) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = value.Kind
	}
	return result
}

func pageBoxes(values []redaction.Box, page int) []redaction.Box {
	result := make([]redaction.Box, 0)
	for _, value := range values {
		if value.Page == page {
			result = append(result, value)
		}
	}
	return result
}

func pageRuns(values []redaction.Run, page int) []redaction.Run {
	result := make([]redaction.Run, 0)
	for _, value := range values {
		if value.Page == page {
			result = append(result, value)
		}
	}
	return result
}

func endorsementTestHash(value string) string {
	return endorsementTestDigest([]byte(value))
}

func endorsementTestDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
